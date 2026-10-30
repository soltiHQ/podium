package durable

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// samePages fails unless a scan returns want rows, the rows of the listing
// drained page by page.
func samePages[T, F any](t *testing.T, name string, want int, filter F, scan func(context.Context, F) ([]T, error), list func(context.Context, F, storage.ListOptions) (*storage.ListResult[T], error)) {
	t.Helper()
	ctx := context.Background()
	paged, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.ListResult[T], error) { return list(ctx, filter, o) })
	if err != nil {
		t.Fatalf("%s: listing: %v", name, err)
	}
	scanned, err := scan(ctx, filter)
	if err != nil {
		t.Fatalf("%s: scan: %v", name, err)
	}
	if len(scanned) != want || !reflect.DeepEqual(scanned, paged) {
		t.Fatalf("%s: the scan returned %d rows and the listing %d, want %d alike", name, len(scanned), len(paged), want)
	}
}

func TestScannerMatchesCollectPages(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	const rows = 1237 // three pages
	if err := storagetest.Seed(ctx, s, storagetest.Fixture{Agents: rows, HistoryRows: rows}); err != nil {
		t.Fatal(err)
	}

	samePages[*model.Agent, storage.AgentFilter](t, "agents", rows, nil, s.ScanAgents, s.ListAgents)
	samePages[*model.Agent, storage.AgentFilter](t, "agents by label", rows/4+1, inmemory.NewAgentFilter().ByLabel("region", "eu-west"), s.ScanAgents, s.ListAgents)
	samePages[*model.Rollout, storage.RolloutFilter](t, "rollouts", rows*storagetest.RolloutsPerAgent, nil, s.ScanRollouts, s.ListRollouts)
	samePages[*model.Spec, storage.SpecFilter](t, "specs", storagetest.SpecCount, nil, s.ScanSpecs, s.ListSpecs)
	samePages[*model.ExecutionRecord, storage.ExecutionFilter](t, "executions", rows, nil, s.ScanExecutions, s.ListExecutions)
	samePages[*model.ExecutionRecord, storage.ExecutionFilter](t, "failed executions", rows/7+1,
		s.BuildExecutionFilter(storage.ExecutionQueryCriteria{Statuses: []string{"failed"}}), s.ScanExecutions, s.ListExecutions)
}

// A paged scan reads each page in an admission of its own, so a transaction that
// commits between two pages splits it across two states. A scan holds one
// admission and shows one state.
func TestScanIsOneState(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	const agents = 600 // more than the 500 rows of a page
	if err := storagetest.Seed(ctx, s, storagetest.Fixture{Agents: agents}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var writeErr error
	go func() {
		defer close(done)
		for generation := 1; generation <= 12 && writeErr == nil; generation++ {
			writeErr = s.WithTx(ctx, func(tx storage.Storage) error {
				for i := range agents {
					a, err := tx.GetAgent(ctx, storagetest.AgentID(i))
					if err != nil {
						return err
					}
					a.LabelAdd("generation", strconv.Itoa(generation))
					if err = tx.UpsertAgent(ctx, a); err != nil {
						return err
					}
				}
				return nil
			})
		}
	}()

	for scans := 0; ; scans++ {
		rows, err := s.ScanAgents(ctx, nil)
		if err != nil || len(rows) != agents {
			t.Fatalf("scan %d returned %d agents, %v, want %d", scans, len(rows), err, agents)
		}
		seen := make(map[string]bool)
		for _, a := range rows {
			generation, _ := a.Label("generation")
			seen[generation] = true
		}
		if len(seen) != 1 {
			t.Fatalf("scan %d shows agents of %d generations: it spans two states", scans, len(seen))
		}
		select {
		case <-done:
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			return
		default:
		}
	}
}

func TestScansOfClosedStoreAreUnavailable(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, scan := range map[string]func() error{
		"agents":     func() error { _, err := s.ScanAgents(ctx, nil); return err },
		"rollouts":   func() error { _, err := s.ScanRollouts(ctx, nil); return err },
		"specs":      func() error { _, err := s.ScanSpecs(ctx, nil); return err },
		"executions": func() error { _, err := s.ScanExecutions(ctx, nil); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := scan(); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatalf("scan of a closed store = %v, want ErrUnavailable", err)
			}
		})
	}
}
