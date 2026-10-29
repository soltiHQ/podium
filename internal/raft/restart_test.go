package raft

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"google.golang.org/protobuf/proto"
)

func openRestartProfile(t *testing.T, dir string) (*Profile, *inmemory.Store, *event.Hub) {
	t.Helper()
	inner := inmemory.New()
	hub := event.NewHub(zerolog.Nop())
	p, err := New(Config{NodeID: "restart-node", BindAddr: "127.0.0.1:0", DataDir: dir, ElectionTimeout: 50 * time.Millisecond, HeartbeatTimeout: 50 * time.Millisecond}, inner, hub, discovery.NewStatic(nil), zerolog.Nop())
	if err != nil {
		hub.Close()
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !p.Leadership().AmLeader() {
		if time.Now().After(deadline) {
			_ = p.Shutdown()
			hub.Close()
			t.Fatal("restart node did not become leader within two seconds")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p, inner, hub
}

func TestRaftCurrentStateRestoresFromLogAndSnapshot(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "command-log", true: "snapshot"}[compact], func(t *testing.T) {
			dir := t.TempDir()
			p, _, hub := openRestartProfile(t, dir)
			agent, err := model.NewAgent("restart-agent", "persisted", "http://agent")
			if err != nil {
				t.Fatal(err)
			}
			agent.SetOS("linux")
			agent.LabelAdd("env", "test")
			agent.SetCapabilities([]model.RunnerCapability{{Name: "echo", Labels: map[string]string{"tier": "test"}, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Shell"}}}})
			if err := p.Store().UpsertAgent(context.Background(), agent); err != nil {
				_ = p.Shutdown()
				hub.Close()
				t.Fatal(err)
			}
			if compact {
				if err := p.raft.Snapshot().Error(); err != nil {
					_ = p.Shutdown()
					hub.Close()
					t.Fatal(err)
				}
			}
			if err := p.Shutdown(); err != nil {
				hub.Close()
				t.Fatal(err)
			}
			hub.Close()
			restored, inner, restoredHub := openRestartProfile(t, dir)
			defer func() { _ = restored.Shutdown() }()
			defer restoredHub.Close()
			// Election precedes asynchronous FSM replay. A leader read barrier must
			// complete before bootstrap or restored reads can consume prior state.
			if err := cluster.AwaitLeaderReady(context.Background(), restored.Leadership()); err != nil {
				t.Fatal(err)
			}
			got, err := inner.GetAgent(context.Background(), agent.ID())
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(wire.AgentToProto(agent), wire.AgentToProto(got)) {
				t.Fatal("current-format state changed during startup preflight/replay")
			}
		})
	}
}

// Unlike the explicit readiness test above, this exercises the first transaction
// directly after election. The store must establish its own applied-state barrier
// before the callback constructs a read view from restored committed state.
func TestRaftFirstTransactionAfterElectionReadsRestoredState(t *testing.T) {
	dir := t.TempDir()
	p, _, hub := openRestartProfile(t, dir)
	agent, err := model.NewAgent("first-tx-agent", "persisted", "http://agent")
	if err != nil {
		_ = p.Shutdown()
		hub.Close()
		t.Fatal(err)
	}
	if err := p.Store().UpsertAgent(context.Background(), agent); err != nil {
		_ = p.Shutdown()
		hub.Close()
		t.Fatal(err)
	}
	if err := p.Shutdown(); err != nil {
		hub.Close()
		t.Fatal(err)
	}
	hub.Close()
	restored, _, restoredHub := openRestartProfile(t, dir)
	defer func() { _ = restored.Shutdown() }()
	defer restoredHub.Close()
	// Deliberately no AwaitLeaderReady/Barrier before this first WithTx call.
	if err := restored.Store().WithTx(context.Background(), func(tx storage.Storage) error {
		got, err := tx.GetAgent(context.Background(), agent.ID())
		if err != nil {
			return err
		}
		if !proto.Equal(wire.AgentToProto(agent), wire.AgentToProto(got)) {
			t.Error("first post-election transaction saw changed state")
		}
		return nil
	}); err != nil {
		t.Fatalf("first post-election transaction saw stale state: %v", err)
	}
}
