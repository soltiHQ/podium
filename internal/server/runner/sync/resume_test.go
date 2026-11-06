package sync

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	agentsvc "github.com/soltiHQ/control-plane/internal/service/agent"
	historysvc "github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// brief is a Leadership whose term is over as it starts: a runner takes
// leadership, does what it does on taking it, and never ticks.
type brief struct{}

func (brief) AmLeader() bool        { return true }
func (brief) CurrentLeader() string { return "" }
func (brief) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	term, cancel := context.WithCancel(ctx)
	cancel()
	return fn(term)
}

// The sync runner captures into the history before it deletes a task, so like the
// collector it has to learn of what another node wrote while it did not lead.
func TestTakingLeadershipRereadsHistoryWrittenMeanwhile(t *testing.T) {
	ctx := context.Background()
	s := inmemory.New()
	svc, err := historysvc.New(s, historysvc.Config{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := svc.Prune(ctx); err != nil || n != 0 {
		t.Fatalf("empty history: removed %d: %v", n, err)
	}
	// Another node leads for a while and writes three rows.
	for attempt := uint32(1); attempt <= 3; attempt++ {
		record, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a", TaskName: "t", TaskUID: "u", Generation: 1, Attempt: attempt, Status: "failed", CapturedAt: time.Now().Add(-time.Duration(attempt) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertExecution(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := svc.Prune(ctx); err != nil || n != 0 {
		t.Fatalf("removed %d rows it had no way to know of: %v", n, err)
	}
	pool := proxy.NewPool(nil, true)
	t.Cleanup(func() { _ = pool.Close() })
	hub := event.NewHub(zerolog.New(io.Discard))
	t.Cleanup(hub.Close)
	r, err := New(Config{}, zerolog.Nop(), s, pool, agentsvc.New(s, zerolog.Nop()), hub, svc, brief{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.Prune(ctx); err != nil || n != 2 {
		t.Fatalf("after the runner took leadership the service removed %d rows, want the 2 over its limit: %v", n, err)
	}
}
