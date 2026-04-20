package raft_test

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	raftpkg "github.com/soltiHQ/control-plane/internal/raft"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// newSingleNode spins a 1-node raft cluster on loopback, returns profile
// and cleanup.
func newSingleNode(t *testing.T) *raftpkg.Profile {
	t.Helper()
	dir := t.TempDir()
	p, err := raftpkg.New(raftpkg.Config{
		NodeID:             "n1",
		BindAddr:           "127.0.0.1:0", // pick an ephemeral port
		DataDir:            dir,
		ElectionTimeout:    50 * time.Millisecond,
		HeartbeatTimeout:   50 * time.Millisecond,
	}, inmemory.New(), discovery.NewStatic(nil), zerolog.Nop())
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown() })

	// Wait to become leader.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Leadership().AmLeader() {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("raft: did not become leader within 2s")
	return nil
}

func TestRaft_AgentUpsertFlowsThroughFSM(t *testing.T) {
	p := newSingleNode(t)
	ctx := context.Background()

	a, err := model.NewAgent("a1", "agent-1", "http://a")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store().UpsertAgent(ctx, a); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	got, err := p.Store().GetAgent(ctx, "a1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.ID() != "a1" || got.Name() != "agent-1" {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestRaft_WithTxBatchFlowsThroughFSM(t *testing.T) {
	p := newSingleNode(t)
	ctx := context.Background()

	a1, _ := model.NewAgent("a1", "agent-1", "http://a1")
	a2, _ := model.NewAgent("a2", "agent-2", "http://a2")

	err := p.Store().WithTx(ctx, func(tx storage.Storage) error {
		if err := tx.UpsertAgent(ctx, a1); err != nil {
			return err
		}
		return tx.UpsertAgent(ctx, a2)
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	for _, id := range []string{"a1", "a2"} {
		if _, err := p.Store().GetAgent(ctx, id); err != nil {
			t.Fatalf("missing %s: %v", id, err)
		}
	}
}

func TestRaft_WithTxReadsOwnWrites(t *testing.T) {
	p := newSingleNode(t)
	ctx := context.Background()

	a, _ := model.NewAgent("a1", "agent-1", "http://a")
	err := p.Store().WithTx(ctx, func(tx storage.Storage) error {
		if err := tx.UpsertAgent(ctx, a); err != nil {
			return err
		}
		// Read-your-writes: the tx view must see the agent we just wrote.
		got, err := tx.GetAgent(ctx, "a1")
		if err != nil {
			return err
		}
		if got.ID() != "a1" {
			t.Fatalf("tx read: want a1, got %s", got.ID())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

func TestRaft_DeleteAgent(t *testing.T) {
	p := newSingleNode(t)
	ctx := context.Background()
	a, _ := model.NewAgent("a1", "agent", "http://a")
	if err := p.Store().UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := p.Store().DeleteAgent(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store().GetAgent(ctx, "a1"); err == nil {
		t.Fatal("agent still present after delete")
	}
}
