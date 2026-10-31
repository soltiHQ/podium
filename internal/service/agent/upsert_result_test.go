package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// syncParams is what agent-a reports in a sync at the given uptime.
func syncParams(uptime int64) model.AgentParams {
	return model.AgentParams{
		ID:                 "agent-a",
		Name:               "Agent A",
		Endpoint:           "http://10.0.0.5:8085",
		EndpointType:       2,
		APIVersion:         1,
		UptimeSeconds:      uptime,
		HeartbeatIntervalS: 30,
		Capabilities: []model.RunnerCapability{{
			Name:      "subprocess",
			Labels:    map[string]string{"zone": "eu"},
			Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Subprocess"}},
		}},
	}
}

// Upsert says what a sync changed in the stored row: whether a field operators
// watch differs (Visible), and whether the agent reconnected (Reconnected): it
// is new, was not active, or reports less uptime than before.
func TestUpsertReportsWhatTheSyncChanged(t *testing.T) {
	setStatus := func(s enum.AgentStatus) func(*testing.T, *inmemory.Store, *Service) {
		return func(t *testing.T, store *inmemory.Store, _ *Service) {
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
		stored bool                                        // the agent has a row before the sync
		before func(*testing.T, *inmemory.Store, *Service) // changes the row
		sync   func(*model.AgentParams)                    // changes the sync, which is syncParams(130) otherwise
		labels map[string]string                           // labels the sync carries itself
		keeps  map[string]string                           // labels the row has afterwards
		want   UpsertResult
	}{
		{name: "new agent", want: UpsertResult{Visible: true, Reconnected: true}},
		{name: "heartbeat", stored: true},
		{name: "heartbeat at the same uptime", stored: true, sync: func(p *model.AgentParams) { p.UptimeSeconds = 100 }},
		{name: "heartbeat with another interval", stored: true, sync: func(p *model.AgentParams) { p.HeartbeatIntervalS = 60 }},
		{name: "heartbeat that reports no interval", stored: true, sync: func(p *model.AgentParams) { p.HeartbeatIntervalS = 0 }},
		{name: "operator labels survive a heartbeat", stored: true, before: func(t *testing.T, _ *inmemory.Store, svc *Service) {
			t.Helper()
			if _, err := svc.PatchLabels(context.Background(), PatchLabels{ID: "agent-a", Labels: map[string]string{"env": "prod"}}); err != nil {
				t.Fatal(err)
			}
		}, keeps: map[string]string{"env": "prod"}},
		{name: "name", stored: true, sync: func(p *model.AgentParams) { p.Name = "Renamed" }, want: UpsertResult{Visible: true}},
		{name: "endpoint", stored: true, sync: func(p *model.AgentParams) { p.Endpoint = "http://10.0.0.6:8085" }, want: UpsertResult{Visible: true}},
		{name: "runner added", stored: true, sync: func(p *model.AgentParams) {
			p.Capabilities = append(p.Capabilities, model.RunnerCapability{Name: "container"})
		}, want: UpsertResult{Visible: true}},
		{name: "runner label", stored: true, sync: func(p *model.AgentParams) { p.Capabilities[0].Labels["zone"] = "us" }, want: UpsertResult{Visible: true}},
		{name: "runner workload", stored: true, sync: func(p *model.AgentParams) { p.Capabilities[0].Workloads[0].Kind = "Wasm" }, want: UpsertResult{Visible: true}},
		{name: "label the sync carries", stored: true, labels: map[string]string{"zone": "eu"}, want: UpsertResult{Visible: true}},
		{name: "inactive agent syncs", stored: true, before: setStatus(enum.AgentStatusInactive), want: UpsertResult{Visible: true, Reconnected: true}},
		{name: "disconnected agent syncs", stored: true, before: setStatus(enum.AgentStatusDisconnected), want: UpsertResult{Visible: true, Reconnected: true}},
		{name: "agent restarted", stored: true, sync: func(p *model.AgentParams) { p.UptimeSeconds = 5 }, want: UpsertResult{Reconnected: true}},
		{name: "agent returns after lifecycle deleted it", stored: true, before: func(t *testing.T, store *inmemory.Store, _ *Service) {
			t.Helper()
			if err := store.DeleteAgent(context.Background(), "agent-a"); err != nil {
				t.Fatal(err)
			}
		}, want: UpsertResult{Visible: true, Reconnected: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := inmemory.New()
			svc := New(store, zerolog.New(io.Discard))
			if tc.stored {
				first, err := model.NewAgentFrom(syncParams(100))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = svc.Upsert(ctx, first); err != nil {
					t.Fatal(err)
				}
			}
			if tc.before != nil {
				tc.before(t, store, svc)
			}

			p := syncParams(130)
			if tc.sync != nil {
				tc.sync(&p)
			}
			next, err := model.NewAgentFrom(p)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.labels {
				next.LabelAdd(k, v)
			}
			got, err := svc.Upsert(ctx, next)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Upsert = %+v, want %+v", got, tc.want)
			}

			// What the sync reported is stored, whether or not it shows.
			stored, err := store.GetAgent(ctx, "agent-a")
			if err != nil {
				t.Fatal(err)
			}
			if stored.UptimeSeconds() != p.UptimeSeconds || stored.LastSeenAt().Before(next.LastSeenAt()) {
				t.Fatalf("stored uptime %d, last seen %v, want those of the sync: %d, %v", stored.UptimeSeconds(), stored.LastSeenAt(), p.UptimeSeconds, next.LastSeenAt())
			}
			for k, v := range tc.keeps {
				if got, ok := stored.Label(k); !ok || got != v {
					t.Fatalf("label %s=%q of the row lost: %v", k, v, stored.LabelsAll())
				}
			}
		})
	}
}

// failingStore fails every transaction.
type failingStore struct {
	storage.Storage
	err error
}

func (f failingStore) WithTx(context.Context, func(storage.Storage) error) error { return f.err }

// A sync that does not reach the store reports nothing, so that nobody sends
// a pulse for a row that was not written.
func TestUpsertFailureReportsNothing(t *testing.T) {
	ctx := context.Background()
	if got, err := New(inmemory.New(), zerolog.Nop()).Upsert(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) || got != (UpsertResult{}) {
		t.Fatalf("Upsert(nil) = %+v, %v, want nothing and ErrInvalidArgument", got, err)
	}
	a, err := model.NewAgentFrom(syncParams(100))
	if err != nil {
		t.Fatal(err)
	}
	svc := New(failingStore{Storage: inmemory.New(), err: storage.ErrUnavailable}, zerolog.Nop())
	if got, err := svc.Upsert(ctx, a); !errors.Is(err, storage.ErrUnavailable) || got != (UpsertResult{}) {
		t.Fatalf("Upsert with the store down = %+v, %v, want nothing and ErrUnavailable", got, err)
	}
}
