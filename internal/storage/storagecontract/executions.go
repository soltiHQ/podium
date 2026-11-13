package storagecontract

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// executions checks the execution records, keyed by a digest of their run: the
// round trip of a full and of a minimal record, an upsert that inserts only a
// new record, the records a store refuses, deletion and the filter the factory
// builds.
func executions(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullExecution(t, "agent-a", "uid-a", 0, minute(0))
		must(t, s.UpsertExecution(ctx, want))

		got, err := s.GetExecution(ctx, want.ID())
		gotRow(t, "GetExecution", got, err, want, equalExecution)
		listed, err := listedExecutions(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListExecutions", listed, []*model.ExecutionRecord{want}, equalExecution)
	})

	t.Run("round trip of a minimal row", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := minimalExecution(t, "agent-a", minute(0))
		must(t, s.UpsertExecution(ctx, want))

		got, err := s.GetExecution(ctx, want.ID())
		gotRow(t, "GetExecution", got, err, want, equalExecution)
		listed, err := listedExecutions(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListExecutions", listed, []*model.ExecutionRecord{want}, equalExecution)
	})

	t.Run("upsert inserts only a new record", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		first := fullExecution(t, "agent-a", "uid-a", 0, minute(0))
		must(t, s.UpsertExecution(ctx, first))

		// A replay of the run, every field but its identity changed, is no error
		// and changes nothing: the first record stays, its capture time included.
		replay := fullExecution(t, "agent-a", "uid-a", 1, minute(5))
		if replay.ID() != first.ID() {
			t.Fatalf("the replay has ID %s, want the first record's %s", replay.ID(), first.ID())
		}
		must(t, s.UpsertExecution(ctx, replay))
		got, err := s.GetExecution(ctx, first.ID())
		gotRow(t, "GetExecution after a replay", got, err, first, equalExecution)

		// Once deleted, the record is new again.
		must(t, s.DeleteExecution(ctx, first.ID()))
		must(t, s.UpsertExecution(ctx, replay))
		got, err = s.GetExecution(ctx, first.ID())
		gotRow(t, "GetExecution after a delete and a replay", got, err, replay, equalExecution)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// A record is built by its validating constructor, which leaves no ID or
		// creation time out: the zero record is the one without either.
		expect(t,
			outcome{"UpsertExecution of nil", s.UpsertExecution(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertExecution of the zero record", s.UpsertExecution(ctx, &model.ExecutionRecord{}), storage.ErrInvalidArgument},
		)
		listed, err := listedExecutions(ctx, s, nil)
		sameIDs(t, "ListExecutions after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		gone, kept := fullExecution(t, "agent-a", "uid-a", 0, minute(0)), fullExecution(t, "agent-a", "uid-b", 0, minute(0))
		missing := fullExecution(t, "agent-a", "uid-x", 0, minute(0)).ID()
		must(t, errors.Join(s.UpsertExecution(ctx, gone), s.UpsertExecution(ctx, kept)))
		expect(t,
			outcome{"DeleteExecution", s.DeleteExecution(ctx, gone.ID()), nil},
			outcome{"GetExecution of a deleted row", errOf(s.GetExecution(ctx, gone.ID())), storage.ErrNotFound},
			outcome{"DeleteExecution of a deleted row", s.DeleteExecution(ctx, gone.ID()), storage.ErrNotFound},
			outcome{"GetExecution of an ID never written", errOf(s.GetExecution(ctx, missing)), storage.ErrNotFound},
			outcome{"DeleteExecution of an ID never written", s.DeleteExecution(ctx, missing), storage.ErrNotFound},
			outcome{"GetExecution of an empty ID", errOf(s.GetExecution(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteExecution of an empty ID", s.DeleteExecution(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetExecution(ctx, kept.ID())
		gotRow(t, "GetExecution of the row kept", got, err, kept, equalExecution)
	})

	t.Run("filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		byName := map[string]*model.ExecutionRecord{}
		for _, r := range []struct{ name, agent, spec, task, uid, status string }{
			{"e1", "agent-a", "spec-a", "build", "uid-1", "succeeded"},
			{"e2", "agent-a", "spec-b", "build", "uid-2", "failed"},
			{"e3", "agent-b", "spec-a", "test", "uid-3", "timeout"},
			// Each key has one of the others as a prefix: an exact comparison tells
			// them apart.
			{"e4", "agent-ab", "spec-ab", "build-all", "uid-34", "canceled"},
		} {
			p := executionParams(r.agent, r.task, r.uid, 0, minute(0))
			p.SpecID, p.Status = r.spec, r.status
			byName[r.name] = newExecution(t, p)
			must(t, s.UpsertExecution(ctx, byName[r.name]))
		}
		ids := func(names ...string) []string {
			out := make([]string, len(names))
			for i, n := range names {
				out[i] = byName[n].ID()
			}
			return out
		}
		listed, err := listedExecutions(ctx, s, nil)
		sameIDs(t, "ListExecutions with no filter", listed, err, ids("e1", "e2", "e3", "e4")...)
		for _, tc := range []struct {
			name string
			c    storage.ExecutionQueryCriteria
			want []string
		}{
			{"empty criteria", storage.ExecutionQueryCriteria{}, ids("e1", "e2", "e3", "e4")},
			{"agent", storage.ExecutionQueryCriteria{AgentID: "agent-a"}, ids("e1", "e2")},
			{"spec", storage.ExecutionQueryCriteria{SpecID: "spec-a"}, ids("e1", "e3")},
			{"task name", storage.ExecutionQueryCriteria{TaskName: "build"}, ids("e1", "e2")},
			{"task UID", storage.ExecutionQueryCriteria{TaskUID: "uid-3"}, ids("e3")},
			{"statuses, any of them", storage.ExecutionQueryCriteria{Statuses: []string{"failed", "timeout"}}, ids("e2", "e3")},
			{"agent and status", storage.ExecutionQueryCriteria{AgentID: "agent-a", Statuses: []string{"succeeded"}}, ids("e1")},
			{"an agent ID in another case", storage.ExecutionQueryCriteria{AgentID: "AGENT-A"}, nil},
			{"a prefix of a task name", storage.ExecutionQueryCriteria{TaskName: "buil"}, nil},
			{"a status no row has", storage.ExecutionQueryCriteria{Statuses: []string{"exhausted"}}, nil},
		} {
			listed, err := listedExecutions(ctx, s, s.BuildExecutionFilter(tc.c))
			sameIDs(t, "ListExecutions, "+tc.name, listed, err, tc.want...)
		}
	})

	t.Run("foreign filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, s.UpsertExecution(ctx, fullExecution(t, "agent-a", "uid-a", 0, minute(0))))
		expect(t,
			outcome{"ListExecutions with a filter of no backend", errOf(s.ListExecutions(ctx, foreignFilter{}, storage.ListOptions{})), storage.ErrInvalidArgument},
			outcome{"ListExecutions with an agent filter", errOf(s.ListExecutions(ctx, s.BuildAgentFilter(storage.AgentQueryCriteria{}), storage.ListOptions{})), storage.ErrInvalidArgument},
		)
	})
}

// listedExecutions drains ListExecutions with filter.
func listedExecutions(ctx context.Context, s storage.Storage, filter storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.ExecutionListResult, error) {
		return s.ListExecutions(ctx, filter, o)
	})
}

// executionParams is the run of taskName and taskUID on agentID, captured at
// captured, with every other field set from v. The fields of the run's identity
// do not depend on v, so the records of two values of v are one run.
func executionParams(agentID, taskName, taskUID string, v int, captured time.Time) model.ExecutionRecordParams {
	code := int32(17 + v)
	return model.ExecutionRecordParams{
		AgentID:               agentID,
		AgentName:             fmt.Sprintf("edge %s %d", agentID, v),
		TaskName:              taskName,
		TaskUID:               taskUID,
		Generation:            3,
		Attempt:               2,
		Status:                []string{"failed", "timeout"}[v%2],
		StartedAtMs:           captured.UnixMilli() - int64(5000+v),
		FinishedAtMs:          captured.UnixMilli() - int64(1000+v),
		ExitCode:              &code,
		Error:                 fmt.Sprintf("exit status %d", code),
		WorkloadAPIVersion:    fmt.Sprintf("solti.io/v%d", v+1),
		WorkloadKind:          []string{"Subprocess", "Container"}[v%2],
		SpecID:                fmt.Sprintf("spec-%d", v),
		SpecName:              fmt.Sprintf("job %d", v),
		SourceResourceVersion: fmt.Sprintf("rv-%d", 40+v),
		CapturedAt:            captured,
	}
}

// newExecution builds the record of p.
func newExecution(t *testing.T, p model.ExecutionRecordParams) *model.ExecutionRecord {
	t.Helper()
	e, err := model.NewExecutionRecord(p)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// fullExecution is the record of task taskUID on agentID with every field set:
// with v 0, none to its zero or default value.
func fullExecution(t *testing.T, agentID, taskUID string, v int, captured time.Time) *model.ExecutionRecord {
	t.Helper()
	return newExecution(t, executionParams(agentID, "build", taskUID, v, captured))
}

// minimalExecution is a record of agentID with the least its constructor takes,
// captured at captured: a run of generation 1 and attempt 1 that succeeded, no
// exit code, no run times and no name but the run's own.
func minimalExecution(t *testing.T, agentID string, captured time.Time) *model.ExecutionRecord {
	t.Helper()
	return newExecution(t, model.ExecutionRecordParams{
		AgentID: agentID, TaskName: "t", TaskUID: "u", Generation: 1, Attempt: 1, Status: "succeeded", CapturedAt: captured,
	})
}

// equalExecution compares every accessor of a record read back with the record
// written. Params holds the fields the other accessors return, one by one.
func equalExecution(t *testing.T, what string, got, want *model.ExecutionRecord) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil execution record, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("AgentID", got.AgentID(), want.AgentID())
	f.same("AgentName", got.AgentName(), want.AgentName())
	f.same("TaskName", got.TaskName(), want.TaskName())
	f.same("TaskUID", got.TaskUID(), want.TaskUID())
	f.same("Generation", got.Generation(), want.Generation())
	f.same("Attempt", got.Attempt(), want.Attempt())
	f.same("Status", got.Status(), want.Status())
	f.same("StartedAtMs", got.StartedAtMs(), want.StartedAtMs())
	f.same("FinishedAtMs", got.FinishedAtMs(), want.FinishedAtMs())
	f.same("ExitCode", exitCode(got), exitCode(want))
	f.same("Error", got.Error(), want.Error())
	f.same("WorkloadAPIVersion", got.WorkloadAPIVersion(), want.WorkloadAPIVersion())
	f.same("WorkloadKind", got.WorkloadKind(), want.WorkloadKind())
	f.same("SpecID", got.SpecID(), want.SpecID())
	f.same("SpecName", got.SpecName(), want.SpecName())
	f.same("SourceResourceVersion", got.SourceResourceVersion(), want.SourceResourceVersion())
	f.same("SizeBytes", got.SizeBytes(), want.SizeBytes())
	f.instant("CapturedAt", got.CapturedAt(), want.CapturedAt())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}

// exitCode is the exit code of e, nil when it has none: a failure prints the
// code, not its address.
func exitCode(e *model.ExecutionRecord) any {
	if c := e.ExitCode(); c != nil {
		return *c
	}
	return nil
}
