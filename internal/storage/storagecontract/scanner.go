package storagecontract

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// scanner checks the optional storage.Scanner: each scan returns what draining
// its listing returns, refuses a foreign filter and a done context, and in a
// transaction scans the transaction.
func scanner(t *testing.T, newStore storeFunc) {
	if _, ok := newStore(t).(storage.Scanner); !ok {
		t.Skip("the store does not implement storage.Scanner")
	}

	// Each scan writes five rows, the first four created two to a minute, and
	// compares itself with its listing for every row, some rows and none.
	keys := []string{"e", "a", "d", "b", "c"}
	t.Run("ScanAgents", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		for i, key := range keys {
			must(t, s.UpsertAgent(ctx, fullAgent(t, "agent-"+key, i%2, minute(i/2))))
		}
		sameScan(t, "ScanAgents", scannerOf(t, s).ScanAgents,
			func(ctx context.Context, f storage.AgentFilter) ([]*model.Agent, error) {
				return listedAgents(ctx, s, f)
			},
			s.BuildAgentFilter(storage.AgentQueryCriteria{Query: "agent-a"}),
			s.BuildAgentFilter(storage.AgentQueryCriteria{Query: "agent-zulu"}),
			equalAgent)
	})

	t.Run("ScanRollouts", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		for i, key := range keys {
			must(t, s.UpsertRollout(ctx, fullRollout(t, "spec-"+key, "agent-a", i%2, minute(i/2))))
		}
		sameScan(t, "ScanRollouts", scannerOf(t, s).ScanRollouts,
			func(ctx context.Context, f storage.RolloutFilter) ([]*model.Rollout, error) {
				return listedRollouts(ctx, s, f)
			},
			s.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: "spec-a"}),
			s.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: "spec-zulu"}),
			equalRollout)
	})

	t.Run("ScanSpecs", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		for i, key := range keys {
			must(t, s.UpsertSpec(ctx, fullSpec(t, "spec-"+key, i%2, minute(i/2))))
		}
		sameScan(t, "ScanSpecs", scannerOf(t, s).ScanSpecs,
			func(ctx context.Context, f storage.SpecFilter) ([]*model.Spec, error) { return listedSpecs(ctx, s, f) },
			s.BuildSpecFilter(storage.SpecQueryCriteria{Query: "spec-a"}),
			s.BuildSpecFilter(storage.SpecQueryCriteria{Query: "spec-zulu"}),
			equalSpec)
	})

	t.Run("ScanExecutions", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		for i, key := range keys {
			must(t, s.UpsertExecution(ctx, fullExecution(t, fmt.Sprintf("agent-%d", i%2), "uid-"+key, i%2, minute(i/2))))
		}
		sameScan(t, "ScanExecutions", scannerOf(t, s).ScanExecutions,
			func(ctx context.Context, f storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
				return listedExecutions(ctx, s, f)
			},
			s.BuildExecutionFilter(storage.ExecutionQueryCriteria{AgentID: "agent-1"}),
			s.BuildExecutionFilter(storage.ExecutionQueryCriteria{AgentID: "agent-zulu"}),
			equalExecution)
	})

	// A scan returns clones the caller owns (storage.Scanner): changing a row
	// through a setter leaves the store's copy as written. The execution record
	// has no setter, so it has no leaf.
	t.Run("a scanned row is a clone the caller owns", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		sc := scannerOf(t, s)
		agent := fullAgent(t, "agent-a", 0, minute(0))
		rollout := fullRollout(t, "spec-a", "agent-a", 0, minute(0))
		spec := fullSpec(t, "spec-a", 0, minute(0))
		must(t, errors.Join(s.UpsertAgent(ctx, agent), s.UpsertRollout(ctx, rollout), s.UpsertSpec(ctx, spec)))

		rows, err := sc.ScanAgents(ctx, nil)
		if err != nil || len(rows) != 1 {
			t.Fatalf("ScanAgents: %d rows, %v, want one", len(rows), err)
		}
		rows[0].SetUpdatedAt(agent.UpdatedAt().Add(time.Hour))
		rows[0].SetStatus(enum.AgentStatusActive)
		again, err := sc.ScanAgents(ctx, nil)
		gotRows(t, "ScanAgents after a change of a scanned row", again, err, []*model.Agent{agent}, equalAgent)
		got, err := s.GetAgent(ctx, agent.ID())
		gotRow(t, "GetAgent after a change of a scanned row", got, err, agent, equalAgent)

		rolloutRows, err := sc.ScanRollouts(ctx, nil)
		if err != nil || len(rolloutRows) != 1 {
			t.Fatalf("ScanRollouts: %d rows, %v, want one", len(rolloutRows), err)
		}
		rolloutRows[0].SetUpdatedAt(rollout.UpdatedAt().Add(time.Hour))
		rolloutRows[0].SetActualTaskID("changed")
		rolloutsAgain, err := sc.ScanRollouts(ctx, nil)
		gotRows(t, "ScanRollouts after a change of a scanned row", rolloutsAgain, err, []*model.Rollout{rollout}, equalRollout)
		gotRollout, err := s.GetRollout(ctx, rollout.ID())
		gotRow(t, "GetRollout after a change of a scanned row", gotRollout, err, rollout, equalRollout)

		specRows, err := sc.ScanSpecs(ctx, nil)
		if err != nil || len(specRows) != 1 {
			t.Fatalf("ScanSpecs: %d rows, %v, want one", len(specRows), err)
		}
		specRows[0].SetUpdatedAt(spec.UpdatedAt().Add(time.Hour))
		specRows[0].SetVersion(spec.Version() + 1)
		specsAgain, err := sc.ScanSpecs(ctx, nil)
		gotRows(t, "ScanSpecs after a change of a scanned row", specsAgain, err, []*model.Spec{spec}, equalSpec)
		gotSpec, err := s.GetSpec(ctx, spec.ID())
		gotRow(t, "GetSpec after a change of a scanned row", gotSpec, err, spec, equalSpec)
	})

	t.Run("foreign filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		sc := scannerOf(t, s)
		expect(t,
			outcome{"ScanAgents with a filter of no backend", errOf(sc.ScanAgents(ctx, foreignFilter{})), storage.ErrInvalidArgument},
			outcome{"ScanRollouts with a filter of no backend", errOf(sc.ScanRollouts(ctx, foreignFilter{})), storage.ErrInvalidArgument},
			outcome{"ScanSpecs with a filter of no backend", errOf(sc.ScanSpecs(ctx, foreignFilter{})), storage.ErrInvalidArgument},
			outcome{"ScanExecutions with a filter of no backend", errOf(sc.ScanExecutions(ctx, foreignFilter{})), storage.ErrInvalidArgument},
		)
	})

	t.Run("done context", func(t *testing.T) {
		s := newStore(t)
		sc, done := scannerOf(t, s), doneContext(t)
		expect(t,
			outcome{"ScanAgents with a done context", errOf(sc.ScanAgents(done, nil)), context.Canceled},
			outcome{"ScanRollouts with a done context", errOf(sc.ScanRollouts(done, nil)), context.Canceled},
			outcome{"ScanSpecs with a done context", errOf(sc.ScanSpecs(done, nil)), context.Canceled},
			outcome{"ScanExecutions with a done context", errOf(sc.ScanExecutions(done, nil)), context.Canceled},
		)
	})

	t.Run("the transaction handle scans the transaction", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		gone, added := newWorld(t, "a", 0), newWorld(t, "c", 0)
		must(t, gone.write(ctx, s))
		err := s.WithTx(ctx, func(tx storage.Storage) error {
			sc, ok := tx.(storage.Scanner)
			if !ok {
				return errors.New("the transaction handle does not implement storage.Scanner, as the store does")
			}
			if err := errors.Join(gone.remove(ctx, tx), added.write(ctx, tx)); err != nil {
				return err
			}
			agents, err := sc.ScanAgents(ctx, nil)
			gotRows(t, "ScanAgents inside the transaction", agents, err, []*model.Agent{added.agent}, equalAgent)
			rollouts, err := sc.ScanRollouts(ctx, nil)
			gotRows(t, "ScanRollouts inside the transaction", rollouts, err, []*model.Rollout{added.rollout}, equalRollout)
			specs, err := sc.ScanSpecs(ctx, nil)
			gotRows(t, "ScanSpecs inside the transaction", specs, err, []*model.Spec{added.spec}, equalSpec)
			executions, err := sc.ScanExecutions(ctx, nil)
			gotRows(t, "ScanExecutions inside the transaction", executions, err, []*model.ExecutionRecord{added.execution}, equalExecution)
			return errRollback
		})
		expect(t, outcome{"WithTx", err, errRollback})
	})
}

// scannerOf returns s as the storage.Scanner the group's first store was.
func scannerOf(t *testing.T, s storage.Storage) storage.Scanner {
	t.Helper()
	sc, ok := s.(storage.Scanner)
	if !ok {
		t.Fatal("the store does not implement storage.Scanner, as the first store of the group did")
	}
	return sc
}

// gotRows compares the rows a read returned with the rows written, in order.
func gotRows[T row](t *testing.T, what string, got []T, err error, want []T, equal func(*testing.T, string, T, T)) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	equalRows(t, what, got, want, equal)
}

// sameScan compares a scan with its listing drained: for every row, for some
// rows and for none. The scan returns the same rows in the same order, each
// field equal, and nil when none matches.
func sameScan[T row, F any](t *testing.T, name string, scan, list func(context.Context, F) ([]T, error), some, none F, equal func(*testing.T, string, T, T)) {
	t.Helper()
	ctx := t.Context()
	var every F
	all, err := list(ctx, every)
	must(t, err)
	for _, tc := range []struct {
		name   string
		filter F
		count  func(n int) bool
	}{
		{"every row", every, func(n int) bool { return n == len(all) && n > 0 }},
		{"some rows", some, func(n int) bool { return n > 0 && n < len(all) }},
		{"no row", none, func(n int) bool { return n == 0 }},
	} {
		what := name + ", " + tc.name
		want, err := list(ctx, tc.filter)
		must(t, err)
		if !tc.count(len(want)) {
			t.Fatalf("%s: the listing returned %d of %d rows; the filter does not fit the check", what, len(want), len(all))
		}
		got, err := scan(ctx, tc.filter)
		gotRows(t, what, got, err, want, equal)
		if err == nil && len(want) == 0 && got != nil {
			t.Errorf("%s: an empty list, want nil", what)
		}
	}
}
