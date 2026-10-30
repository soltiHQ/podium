package raft

import (
	"context"
	"reflect"
	"testing"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// countingInner counts the agent reads that reach the local state.
type countingInner struct {
	*inmemory.Store
	scans, lists int
}

func (c *countingInner) ScanAgents(ctx context.Context, f storage.AgentFilter) ([]*model.Agent, error) {
	c.scans++
	return c.Store.ScanAgents(ctx, f)
}

func (c *countingInner) ListAgents(ctx context.Context, f storage.AgentFilter, o storage.ListOptions) (*storage.AgentListResult, error) {
	c.lists++
	return c.Store.ListAgents(ctx, f, o)
}

func TestScansReadTheLocalState(t *testing.T) {
	ctx := context.Background()
	const rows = 600 // more than the 500 rows of a page
	inner := &countingInner{Store: inmemory.New()}
	if err := storagetest.Seed(ctx, inner, storagetest.Fixture{Agents: rows, HistoryRows: rows}); err != nil {
		t.Fatal(err)
	}
	// Reads never reach the Raft instance, so the store does not need one.
	s := &Store{inner: inner}

	agents, err := storage.AllAgents(ctx, s, nil)
	if err != nil || len(agents) != rows {
		t.Fatalf("AllAgents = %d agents, %v, want %d", len(agents), err, rows)
	}
	if inner.scans != 1 || inner.lists != 0 {
		t.Fatalf("AllAgents reached the local state with %d scans and %d listings, want one scan", inner.scans, inner.lists)
	}

	paged, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.AgentListResult, error) { return s.ListAgents(ctx, nil, o) })
	if err != nil || !reflect.DeepEqual(agents, paged) {
		t.Fatalf("the scan and the listing differ: %d and %d agents, %v", len(agents), len(paged), err)
	}

	rollouts, err := storage.AllRollouts(ctx, s, s.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: storagetest.SpecID(0)}))
	if err != nil || len(rollouts) == 0 {
		t.Fatalf("AllRollouts = %d rollouts, %v, want the rollouts of the spec", len(rollouts), err)
	}
	specs, err := storage.AllSpecs(ctx, s, nil)
	if err != nil || len(specs) != storagetest.SpecCount {
		t.Fatalf("AllSpecs = %d specs, %v, want %d", len(specs), err, storagetest.SpecCount)
	}
	runs, err := storage.AllExecutions(ctx, s, nil)
	if err != nil || len(runs) != rows {
		t.Fatalf("AllExecutions = %d records, %v, want %d", len(runs), err, rows)
	}
}
