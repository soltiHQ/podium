package durable

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// The commit observer hears each commit that appends an entry, once, with a
// duration that is not negative, and none for a transaction that writes nothing.
func TestCommitObserverHearsEachCommit(t *testing.T) {
	var heard []time.Duration
	s, _ := newTestStore(t, WithCommitObserver(func(d time.Duration) { heard = append(heard, d) }))
	ctx := context.Background()
	for i := range 3 {
		a, err := model.NewAgent(fmt.Sprintf("a%d", i), "n", "http://127.0.0.1:9000")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if len(heard) != 3 {
		t.Fatalf("observer heard %d commits, want 3", len(heard))
	}
	for _, d := range heard {
		if d <= 0 {
			t.Fatalf("a commit took %v", d)
		}
	}
	if err := s.WithTx(ctx, func(tx storage.Storage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(heard) != 3 {
		t.Fatalf("an empty transaction was reported: %d commits", len(heard))
	}
}

// The counts read the published state, as a scan of it tallies, and fail with the
// store once it has failed.
func TestCountsReadThePublishedState(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for i, st := range []enum.AgentStatus{enum.AgentStatusActive, enum.AgentStatusInactive, enum.AgentStatusInactive} {
		a, err := model.NewAgent(fmt.Sprintf("a%d", i), "n", "http://127.0.0.1:9000")
		if err != nil {
			t.Fatal(err)
		}
		a.MarkStatus(st)
		if err = s.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := s.CountAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[enum.AgentStatusActive] != 1 || counts[enum.AgentStatusInactive] != 2 || counts[enum.AgentStatusDisconnected] != 0 {
		t.Fatalf("CountAgents %v, want 1 active and 2 inactive", counts)
	}
	_ = s.fail(fmt.Errorf("injected"))
	if _, err := s.CountAgents(ctx); err == nil {
		t.Fatal("a failed store still counts")
	}
	if _, err := s.CountRollouts(ctx); err == nil {
		t.Fatal("a failed store still counts rollouts")
	}
}
