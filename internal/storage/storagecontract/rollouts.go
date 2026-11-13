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

// rollouts checks the rollout rows, keyed by spec and agent: the round trip of
// a full and of a minimal row, replacement, the rows a store refuses, deletion,
// the filter the factory builds and the deletion of a spec's rollouts.
func rollouts(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullRollout(t, "spec-a", "agent-a", 0, minute(0))
		must(t, s.UpsertRollout(ctx, want))

		got, err := s.GetRollout(ctx, want.ID())
		gotRow(t, "GetRollout", got, err, want, equalRollout)
		listed, err := listedRollouts(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListRollouts", listed, []*model.Rollout{want}, equalRollout)
	})

	t.Run("round trip of a minimal row", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := minimalRollout(t, "spec-a", "agent-a", minute(0))
		must(t, s.UpsertRollout(ctx, want))

		got, err := s.GetRollout(ctx, want.ID())
		gotRow(t, "GetRollout", got, err, want, equalRollout)
		listed, err := listedRollouts(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListRollouts", listed, []*model.Rollout{want}, equalRollout)
	})

	t.Run("upsert replaces", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		first, other := fullRollout(t, "spec-a", "agent-a", 0, minute(0)), fullRollout(t, "spec-b", "agent-a", 0, minute(3))
		must(t, errors.Join(s.UpsertRollout(ctx, first), s.UpsertRollout(ctx, other)))

		// Every field but the keys changes, the creation time too: the row lists at
		// its new one, and the filter sees its new status, not the old. That
		// CreatedAt never changes is a rule for callers, not the store, which keeps
		// no earlier one.
		want := fullRollout(t, "spec-a", "agent-a", 1, minute(5))
		must(t, s.UpsertRollout(ctx, want))
		got, err := s.GetRollout(ctx, want.ID())
		gotRow(t, "GetRollout", got, err, want, equalRollout)
		listed, err := listedRollouts(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListRollouts", listed, []*model.Rollout{want, other}, equalRollout)
		listed, err = listedRollouts(ctx, s, s.BuildRolloutFilter(storage.RolloutQueryCriteria{Statuses: []enum.SyncStatus{want.Status()}}))
		sameIDs(t, "ListRollouts by the new status", listed, err, want.ID())
		listed, err = listedRollouts(ctx, s, s.BuildRolloutFilter(storage.RolloutQueryCriteria{Statuses: []enum.SyncStatus{first.Status()}}))
		sameIDs(t, "ListRollouts by the old status", listed, err, other.ID())
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.Rollout{}
		noID.SetCreatedAt(minute(0))
		noCreation := fullRollout(t, "spec-a", "agent-a", 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"UpsertRollout of nil", s.UpsertRollout(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertRollout of a row without an ID", s.UpsertRollout(ctx, noID), storage.ErrInvalidArgument},
			outcome{"UpsertRollout of a row without a creation time", s.UpsertRollout(ctx, noCreation), storage.ErrInvalidArgument},
		)
		listed, err := listedRollouts(ctx, s, nil)
		sameIDs(t, "ListRollouts after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		gone, kept := fullRollout(t, "spec-a", "agent-a", 0, minute(0)), fullRollout(t, "spec-b", "agent-a", 0, minute(0))
		missing := model.RolloutID("spec-x", "agent-a")
		must(t, errors.Join(s.UpsertRollout(ctx, gone), s.UpsertRollout(ctx, kept)))
		expect(t,
			outcome{"DeleteRollout", s.DeleteRollout(ctx, gone.ID()), nil},
			outcome{"GetRollout of a deleted row", errOf(s.GetRollout(ctx, gone.ID())), storage.ErrNotFound},
			outcome{"DeleteRollout of a deleted row", s.DeleteRollout(ctx, gone.ID()), storage.ErrNotFound},
			outcome{"GetRollout of an ID never written", errOf(s.GetRollout(ctx, missing)), storage.ErrNotFound},
			outcome{"DeleteRollout of an ID never written", s.DeleteRollout(ctx, missing), storage.ErrNotFound},
			outcome{"GetRollout of an empty ID", errOf(s.GetRollout(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteRollout of an empty ID", s.DeleteRollout(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetRollout(ctx, kept.ID())
		gotRow(t, "GetRollout of the row kept", got, err, kept, equalRollout)
	})

	t.Run("filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		var (
			pending = rolloutIn(t, "spec-a", "agent-a", enum.SyncStatusPending)
			synced  = rolloutIn(t, "spec-a", "agent-b", enum.SyncStatusSynced)
			failed  = rolloutIn(t, "spec-b", "agent-a", enum.SyncStatusFailed)
			// Its spec ID has spec-a's as a prefix: an exact comparison tells them apart.
			longer = rolloutIn(t, "spec-ab", "agent-a", enum.SyncStatusSynced)
			// Agent IDs that differ from agent-a by a suffix or in case: an exact
			// comparison tells them apart.
			longerAgent = rolloutIn(t, "spec-c", "agent-ab", enum.SyncStatusPending)
			upperAgent  = rolloutIn(t, "spec-c", "AGENT-A", enum.SyncStatusPending)
		)
		every := []string{pending.ID(), synced.ID(), failed.ID(), longer.ID(), longerAgent.ID(), upperAgent.ID()}
		must(t, errors.Join(
			s.UpsertRollout(ctx, pending), s.UpsertRollout(ctx, synced), s.UpsertRollout(ctx, failed),
			s.UpsertRollout(ctx, longer), s.UpsertRollout(ctx, longerAgent), s.UpsertRollout(ctx, upperAgent),
		))
		listed, err := listedRollouts(ctx, s, nil)
		sameIDs(t, "ListRollouts with no filter", listed, err, every...)
		for _, tc := range []struct {
			name string
			c    storage.RolloutQueryCriteria
			want []string
		}{
			{"empty criteria", storage.RolloutQueryCriteria{}, every},
			{"spec", storage.RolloutQueryCriteria{SpecID: "spec-a"}, []string{pending.ID(), synced.ID()}},
			{"agent", storage.RolloutQueryCriteria{AgentID: "agent-a"}, []string{pending.ID(), failed.ID(), longer.ID()}},
			{"one status", storage.RolloutQueryCriteria{Statuses: []enum.SyncStatus{enum.SyncStatusSynced}}, []string{synced.ID(), longer.ID()}},
			{"statuses, any of them", storage.RolloutQueryCriteria{Statuses: []enum.SyncStatus{enum.SyncStatusPending, enum.SyncStatusFailed}}, []string{pending.ID(), failed.ID(), longerAgent.ID(), upperAgent.ID()}},
			{"spec and agent", storage.RolloutQueryCriteria{SpecID: "spec-a", AgentID: "agent-a"}, []string{pending.ID()}},
			{"spec and status", storage.RolloutQueryCriteria{SpecID: "spec-a", Statuses: []enum.SyncStatus{enum.SyncStatusSynced}}, []string{synced.ID()}},
			{"a spec ID in another case", storage.RolloutQueryCriteria{SpecID: "SPEC-A"}, nil},
			{"an agent ID in another case", storage.RolloutQueryCriteria{AgentID: "AGENT-A"}, []string{upperAgent.ID()}},
			{"an agent ID with a prefix of another's", storage.RolloutQueryCriteria{AgentID: "agent-ab"}, []string{longerAgent.ID()}},
			{"a status no row has", storage.RolloutQueryCriteria{Statuses: []enum.SyncStatus{enum.SyncStatusDrift}}, nil},
		} {
			listed, err := listedRollouts(ctx, s, s.BuildRolloutFilter(tc.c))
			sameIDs(t, "ListRollouts, "+tc.name, listed, err, tc.want...)
		}
	})

	t.Run("foreign filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, s.UpsertRollout(ctx, fullRollout(t, "spec-a", "agent-a", 0, minute(0))))
		expect(t,
			outcome{"ListRollouts with a filter of no backend", errOf(s.ListRollouts(ctx, foreignFilter{}, storage.ListOptions{})), storage.ErrInvalidArgument},
			outcome{"ListRollouts with a spec filter", errOf(s.ListRollouts(ctx, s.BuildSpecFilter(storage.SpecQueryCriteria{}), storage.ListOptions{})), storage.ErrInvalidArgument},
		)
	})

	t.Run("delete by spec", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		var (
			gone1 = fullRollout(t, "spec-a", "agent-a", 0, minute(0))
			gone2 = fullRollout(t, "spec-a", "agent-b", 0, minute(0))
			// Spec IDs that differ from spec-a by a suffix or in case: an exact
			// comparison keeps their rollouts.
			kept1 = fullRollout(t, "spec-ab", "agent-a", 0, minute(0))
			kept2 = fullRollout(t, "SPEC-A", "agent-a", 0, minute(0))
		)
		must(t, errors.Join(s.UpsertRollout(ctx, gone1), s.UpsertRollout(ctx, gone2), s.UpsertRollout(ctx, kept1), s.UpsertRollout(ctx, kept2)))
		must(t, s.DeleteRolloutsBySpec(ctx, "spec-a"))
		listed, err := listedRollouts(ctx, s, nil)
		sameIDs(t, "ListRollouts after DeleteRolloutsBySpec", listed, err, kept1.ID(), kept2.ID())
		expect(t,
			outcome{"DeleteRolloutsBySpec again", s.DeleteRolloutsBySpec(ctx, "spec-a"), nil},
			outcome{"DeleteRolloutsBySpec of a spec with no rollout", s.DeleteRolloutsBySpec(ctx, "spec-x"), nil},
			outcome{"DeleteRolloutsBySpec of an empty spec ID", s.DeleteRolloutsBySpec(ctx, ""), storage.ErrInvalidArgument},
		)
		listed, err = listedRollouts(ctx, s, nil)
		sameIDs(t, "ListRollouts after the repeated deletes", listed, err, kept1.ID(), kept2.ID())
	})
}

// listedRollouts drains ListRollouts with filter.
func listedRollouts(ctx context.Context, s storage.Storage, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.RolloutListResult, error) {
		return s.ListRollouts(ctx, filter, o)
	})
}

// fullRollout is the rollout of specID on agentID with every field set from v:
// with v 0, none to its zero or default value.
func fullRollout(t *testing.T, specID, agentID string, v int, created time.Time) *model.Rollout {
	t.Helper()
	r, err := model.NewRollout(specID, agentID, 5+v)
	if err != nil {
		t.Fatal(err)
	}
	r.SetActualTaskID(fmt.Sprintf("task-%s-%d", specID, v))
	r.RestoreState(
		[]enum.SyncStatus{enum.SyncStatusFailed, enum.SyncStatusDrift}[v%2],
		[]enum.RolloutIntent{enum.RolloutIntentUpdate, enum.RolloutIntentUninstall}[v%2],
		4+v, 2+v, fmt.Sprintf("apply failed %d", v),
		created.Add(time.Duration(v+1)*time.Minute), created.Add(time.Duration(v+2)*time.Minute),
	)
	r.SetCreatedAt(created)
	r.SetUpdatedAt(created.Add(time.Duration(v+3) * time.Minute))
	return r
}

// minimalRollout is the rollout of specID on agentID as its constructor builds
// it, at desired generation 0, created at created: every other field zero or
// the constructor's default, its push, sync and modification times zero.
func minimalRollout(t *testing.T, specID, agentID string, created time.Time) *model.Rollout {
	t.Helper()
	r, err := model.NewRollout(specID, agentID, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.SetCreatedAt(created)
	r.SetUpdatedAt(time.Time{})
	return r
}

// rolloutIn is fullRollout with status, created at minute 0.
func rolloutIn(t *testing.T, specID, agentID string, status enum.SyncStatus) *model.Rollout {
	t.Helper()
	r := fullRollout(t, specID, agentID, 0, minute(0))
	r.RestoreState(status, r.Intent(), r.ObservedGeneration(), r.Attempts(), r.Error(), r.LastPushedAt(), r.LastSyncedAt())
	return r
}

// equalRollout compares every accessor of a rollout read back with the rollout
// written.
func equalRollout(t *testing.T, what string, got, want *model.Rollout) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil rollout, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("SpecID", got.SpecID(), want.SpecID())
	f.same("AgentID", got.AgentID(), want.AgentID())
	f.same("DesiredGeneration", got.DesiredGeneration(), want.DesiredGeneration())
	f.same("ObservedGeneration", got.ObservedGeneration(), want.ObservedGeneration())
	f.same("Status", got.Status(), want.Status())
	f.same("Intent", got.Intent(), want.Intent())
	f.same("ActualTaskID", got.ActualTaskID(), want.ActualTaskID())
	f.same("Error", got.Error(), want.Error())
	f.same("Attempts", got.Attempts(), want.Attempts())
	f.instant("LastPushedAt", got.LastPushedAt(), want.LastPushedAt())
	f.instant("LastSyncedAt", got.LastSyncedAt(), want.LastSyncedAt())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
