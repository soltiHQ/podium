package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"

	agentv1 "github.com/soltiHQ/control-plane/api/gen/solti/agent/v1"
	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// heartbeatBuckets let an agent heartbeat far more often than podium lets it:
// these tests count what many heartbeats cause, not how fast they may come.
func heartbeatBuckets() *ratelimit.Buckets {
	return ratelimit.NewBuckets(ratelimit.BucketConfig{Burst: 1000, Refill: time.Second})
}

// syncTransports are the two discovery transports. Each builds a handler over
// svc and hub and returns the function that syncs one heartbeat through it.
var syncTransports = []struct {
	name string
	sync func(svc *agent.Service, hub *event.Hub) func(*discoverv1.SyncRequest) error
}{
	{"http", func(svc *agent.Service, hub *event.Hub) func(*discoverv1.SyncRequest) error {
		h := NewHTTPDiscovery(zerolog.Nop(), svc, hub, discoveryLimiter(), heartbeatBuckets(), nil, false)
		return func(req *discoverv1.SyncRequest) error {
			body, err := discoverySyncMarshal.Marshal(req)
			if err != nil {
				return err
			}
			if res := syncHTTP(h, string(body), ""); res.Code != http.StatusOK {
				return fmt.Errorf("HTTP %d %s", res.Code, res.Body.String())
			}
			return nil
		}
	}},
	{"grpc", func(svc *agent.Service, hub *event.Hub) func(*discoverv1.SyncRequest) error {
		g := NewGRPCDiscovery(zerolog.Nop(), svc, hub, discoveryLimiter(), heartbeatBuckets(), nil, false)
		return func(req *discoverv1.SyncRequest) error {
			_, err := g.Sync(context.Background(), req)
			return err
		}
	}},
}

// heartbeat is a sync of agent-a at the given uptime, which mutate may change
// before it goes out.
func heartbeat(uptime int64, mutate func(*discoverv1.SyncRequest)) *discoverv1.SyncRequest {
	req := &discoverv1.SyncRequest{
		Id:                 "agent-a",
		Name:               "Agent A",
		Endpoint:           "10.0.0.5:50051",
		EndpointType:       discoverv1.EndpointType_ENDPOINT_TYPE_GRPC,
		ApiVersion:         1,
		UptimeSeconds:      uptime,
		HeartbeatIntervalS: 30,
		Os:                 "linux",
		Arch:               "amd64",
		Platform:           "ubuntu",
		Capabilities: &agentv1.AgentCapabilities{Runners: []*agentv1.RunnerCapability{{
			Name:      "subprocess",
			Labels:    map[string]string{"zone": "eu"},
			Workloads: []*agentv1.WorkloadType{{ApiVersion: "solti.io/v1", Kind: "Subprocess"}},
		}}},
	}
	if mutate != nil {
		mutate(req)
	}
	return req
}

func mustSync(t *testing.T, sync func(*discoverv1.SyncRequest) error, req *discoverv1.SyncRequest) {
	t.Helper()
	if err := sync(req); err != nil {
		t.Fatalf("sync at uptime %d: %v", req.GetUptimeSeconds(), err)
	}
}

// agentUpdates drains ch and counts the agent_update pulses in it. The hub
// delivers a pulse before the sync that sent it returns, so none arrives late.
func agentUpdates(ch <-chan string) (n int) {
	for {
		select {
		case ev := <-ch:
			if ev == event.RefreshAgents {
				n++
			}
		default:
			return n
		}
	}
}

// A heartbeat that changes nothing but the time of the sync and the uptime
// sends no agent_update. Every pulse makes each open agents, dashboard and spec
// page fetch again, and the spec page recompute its placement, so a pulse per
// heartbeat is a storm that grows with the fleet.
func TestHeartbeatWithoutChangeSendsNoAgentUpdate(t *testing.T) {
	for _, tr := range syncTransports {
		t.Run(tr.name, func(t *testing.T) {
			var (
				ctx   = context.Background()
				svc   = agent.New(inmemory.New(), zerolog.Nop())
				hub   = event.NewHub(zerolog.Nop())
				sync  = tr.sync(svc, hub)
				first = heartbeat(100, nil)
			)
			defer hub.Close()
			pulses := hub.Subscribe(t.Context())

			// The agent appears: that is visible.
			mustSync(t, sync, first)
			if n := agentUpdates(pulses); n != 1 {
				t.Fatalf("first sync of a new agent: %d agent_update pulses, want 1", n)
			}
			stored, err := svc.Get(ctx, "agent-a")
			if err != nil {
				t.Fatal(err)
			}

			const beats = 10
			for i := int64(1); i <= beats; i++ {
				mustSync(t, sync, heartbeat(first.GetUptimeSeconds()+30*i, nil))
			}
			if n := agentUpdates(pulses); n != 0 {
				t.Fatalf("%d agent_update pulses for %d heartbeats that changed nothing visible, want none", n, beats)
			}

			// The heartbeats were stored all the same.
			last, err := svc.Get(ctx, "agent-a")
			if err != nil {
				t.Fatal(err)
			}
			if want := first.GetUptimeSeconds() + 30*beats; last.UptimeSeconds() != want || last.LastSeenAt().Before(stored.LastSeenAt()) {
				t.Fatalf("stored uptime %d, last seen %v after %v, want the last heartbeat at uptime %d", last.UptimeSeconds(), last.LastSeenAt(), stored.LastSeenAt(), want)
			}
		})
	}
}

// A sync that changes what the agent pages show sends one agent_update, and so
// does an agent that reconnects. The heartbeat after it is the stored row
// again and sends none. The status changes are what lifecycle does to an agent
// that fell silent; a sync makes it active again.
func TestVisibleChangeSendsAgentUpdate(t *testing.T) {
	setStatus := func(s enum.AgentStatus) func(t *testing.T, store *inmemory.Store) {
		return func(t *testing.T, store *inmemory.Store) {
			t.Helper()
			ag, err := store.GetAgent(context.Background(), "agent-a")
			if err != nil {
				t.Fatal(err)
			}
			ag.MarkStatus(s)
			if err = store.UpsertAgent(context.Background(), ag); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, store *inmemory.Store) // the state the sync finds
		change func(*discoverv1.SyncRequest)             // what the sync reports differently
		uptime int64                                     // zero: 30 s more than the first sync
	}{
		{name: "name", change: func(r *discoverv1.SyncRequest) { r.Name = "Renamed" }},
		{name: "endpoint", change: func(r *discoverv1.SyncRequest) { r.Endpoint = "10.0.0.6:50051" }},
		{name: "runner added", change: func(r *discoverv1.SyncRequest) {
			r.Capabilities.Runners = append(r.Capabilities.Runners, &agentv1.RunnerCapability{Name: "container"})
		}},
		{name: "runner label", change: func(r *discoverv1.SyncRequest) { r.Capabilities.Runners[0].Labels["zone"] = "us" }},
		{name: "runner workload", change: func(r *discoverv1.SyncRequest) { r.Capabilities.Runners[0].Workloads[0].Kind = "Wasm" }},
		{name: "went inactive", before: setStatus(enum.AgentStatusInactive)},
		{name: "went disconnected", before: setStatus(enum.AgentStatusDisconnected)},
		{name: "came back after deletion", before: func(t *testing.T, store *inmemory.Store) {
			t.Helper()
			if err := store.DeleteAgent(context.Background(), "agent-a"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "restarted", uptime: 5},
	} {
		for _, tr := range syncTransports {
			t.Run(tr.name+"/"+tc.name, func(t *testing.T) {
				var (
					store = inmemory.New()
					svc   = agent.New(store, zerolog.Nop())
					hub   = event.NewHub(zerolog.Nop())
					sync  = tr.sync(svc, hub)
				)
				defer hub.Close()
				pulses := hub.Subscribe(t.Context())

				mustSync(t, sync, heartbeat(100, nil))
				agentUpdates(pulses)
				if tc.before != nil {
					tc.before(t, store)
				}

				uptime := tc.uptime
				if uptime == 0 {
					uptime = 130
				}
				mustSync(t, sync, heartbeat(uptime, tc.change))
				if n := agentUpdates(pulses); n != 1 {
					t.Fatalf("%d agent_update pulses for the sync, want 1", n)
				}
				mustSync(t, sync, heartbeat(uptime+30, tc.change))
				if n := agentUpdates(pulses); n != 0 {
					t.Fatalf("%d agent_update pulses for the heartbeat after it, want none", n)
				}
			})
		}
	}
}
