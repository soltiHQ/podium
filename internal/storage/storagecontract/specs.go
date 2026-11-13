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

// specs checks the spec rows: the round trip, replacement, the rows a store
// refuses, deletion and the filter the factory builds.
func specs(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullSpec(t, "spec-a", 0, minute(0))
		must(t, s.UpsertSpec(ctx, want))

		got, err := s.GetSpec(ctx, want.ID())
		gotRow(t, "GetSpec", got, err, want, equalSpec)
		listed, err := listedSpecs(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListSpecs", listed, []*model.Spec{want}, equalSpec)
	})

	t.Run("upsert replaces", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		first, other := fullSpec(t, "spec-a", 0, minute(0)), fullSpec(t, "spec-b", 0, minute(3))
		must(t, errors.Join(s.UpsertSpec(ctx, first), s.UpsertSpec(ctx, other)))

		// Every field changes, the creation time too: the row lists at its new one,
		// and the filter sees its new name, not the old.
		want := fullSpec(t, "spec-a", 1, minute(5))
		must(t, s.UpsertSpec(ctx, want))
		got, err := s.GetSpec(ctx, want.ID())
		gotRow(t, "GetSpec", got, err, want, equalSpec)
		listed, err := listedSpecs(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListSpecs", listed, []*model.Spec{want, other}, equalSpec)
		listed, err = listedSpecs(ctx, s, s.BuildSpecFilter(storage.SpecQueryCriteria{Query: want.Name()}))
		sameIDs(t, "ListSpecs by the new name", listed, err, want.ID())
		listed, err = listedSpecs(ctx, s, s.BuildSpecFilter(storage.SpecQueryCriteria{Query: first.Name()}))
		sameIDs(t, "ListSpecs by the old name", listed, err)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.Spec{}
		noID.SetCreatedAt(minute(0))
		noCreation := fullSpec(t, "spec-a", 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"UpsertSpec of nil", s.UpsertSpec(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertSpec of a row without an ID", s.UpsertSpec(ctx, noID), storage.ErrInvalidArgument},
			outcome{"UpsertSpec of a row without a creation time", s.UpsertSpec(ctx, noCreation), storage.ErrInvalidArgument},
		)
		listed, err := listedSpecs(ctx, s, nil)
		sameIDs(t, "ListSpecs after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept := fullSpec(t, "spec-b", 0, minute(0))
		must(t, errors.Join(s.UpsertSpec(ctx, fullSpec(t, "spec-a", 0, minute(0))), s.UpsertSpec(ctx, kept)))
		expect(t,
			outcome{"DeleteSpec", s.DeleteSpec(ctx, "spec-a"), nil},
			outcome{"GetSpec of a deleted row", errOf(s.GetSpec(ctx, "spec-a")), storage.ErrNotFound},
			outcome{"DeleteSpec of a deleted row", s.DeleteSpec(ctx, "spec-a"), storage.ErrNotFound},
			outcome{"GetSpec of an ID never written", errOf(s.GetSpec(ctx, "spec-x")), storage.ErrNotFound},
			outcome{"DeleteSpec of an ID never written", s.DeleteSpec(ctx, "spec-x"), storage.ErrNotFound},
			outcome{"GetSpec of an empty ID", errOf(s.GetSpec(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteSpec of an empty ID", s.DeleteSpec(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetSpec(ctx, kept.ID())
		gotRow(t, "GetSpec of the row kept", got, err, kept, equalSpec)
	})

	t.Run("filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		for _, r := range []struct{ id, name, slot string }{
			{"spec-kilo", "Lima", "mike-zone1"},
			{"spec-oscar", "papa_quebec", "romeo-zone1"},
			{"spec-sierra", "tango 100%", "uniform-zone2"},
			{"spec-victor", "papaxquebec", "whiskey-zone2"},
		} {
			must(t, s.UpsertSpec(ctx, newSpec(t, r.id, r.name, r.slot, 0, minute(0))))
		}
		every := []string{"spec-kilo", "spec-oscar", "spec-sierra", "spec-victor"}
		listed, err := listedSpecs(ctx, s, nil)
		sameIDs(t, "ListSpecs with no filter", listed, err, every...)
		for _, tc := range []struct {
			name string
			c    storage.SpecQueryCriteria
			want []string
		}{
			{"empty criteria", storage.SpecQueryCriteria{}, every},
			{"query on the name, in another case", storage.SpecQueryCriteria{Query: "lIMA"}, []string{"spec-kilo"}},
			{"query on the slot, in another case", storage.SpecQueryCriteria{Query: "MIKE"}, []string{"spec-kilo"}},
			{"query on the ID, not searched", storage.SpecQueryCriteria{Query: "kilo"}, nil},
			{"query matching two rows", storage.SpecQueryCriteria{Query: "zone1"}, []string{"spec-kilo", "spec-oscar"}},
			// The spec is silent on spaces around a query: the in-memory store trims
			// them, so a query of spaces alone is no query.
			{"query with spaces around it", storage.SpecQueryCriteria{Query: "  lima  "}, []string{"spec-kilo"}},
			{"query of spaces", storage.SpecQueryCriteria{Query: "   "}, every},
			{"query with an underscore, matched as one", storage.SpecQueryCriteria{Query: "papa_q"}, []string{"spec-oscar"}},
			{"query with a percent sign, matched as one", storage.SpecQueryCriteria{Query: "%"}, []string{"spec-sierra"}},
		} {
			listed, err := listedSpecs(ctx, s, s.BuildSpecFilter(tc.c))
			sameIDs(t, "ListSpecs, "+tc.name, listed, err, tc.want...)
		}
	})

	t.Run("foreign filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, s.UpsertSpec(ctx, fullSpec(t, "spec-a", 0, minute(0))))
		expect(t,
			outcome{"ListSpecs with a filter of no backend", errOf(s.ListSpecs(ctx, foreignFilter{}, storage.ListOptions{})), storage.ErrInvalidArgument},
			outcome{"ListSpecs with a rollout filter", errOf(s.ListSpecs(ctx, s.BuildRolloutFilter(storage.RolloutQueryCriteria{}), storage.ListOptions{})), storage.ErrInvalidArgument},
		)
	})
}

// listedSpecs drains ListSpecs with filter.
func listedSpecs(ctx context.Context, s storage.Storage, filter storage.SpecFilter) ([]*model.Spec, error) {
	return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.SpecListResult, error) {
		return s.ListSpecs(ctx, filter, o)
	})
}

// newSpec is spec id with name and slot and every other field set from v. Its
// kind config holds only what a JSON document decodes to: strings, float64
// numbers, booleans, lists and objects.
func newSpec(t *testing.T, id, name, slot string, v int, created time.Time) *model.Spec {
	t.Helper()
	s, err := model.NewSpec(id, name, slot)
	if err != nil {
		t.Fatal(err)
	}
	s.SetKindType([]enum.TaskKindType{enum.TaskKindContainer, enum.TaskKindWasm}[v%2])
	s.SetKindConfig(map[string]any{
		"image":      fmt.Sprintf("registry.example.net/job:%d", v),
		"args":       []any{"--shard", fmt.Sprint(v), float64(v) + 0.5},
		"env":        map[string]any{"MODE": fmt.Sprintf("mode-%d", v), "LIMITS": map[string]any{"cpus": float64(v) + 1.5}},
		"privileged": v%2 == 0,
	})
	s.SetTimeoutMs(int64(45000 + v))
	s.SetRestartType([]enum.RestartType{enum.RestartAlways, enum.RestartOnFailure}[v%2])
	s.SetIntervalMs(int64(60000 + v))
	s.SetBackoff(model.BackoffConfig{
		Jitter:  []enum.JitterStrategy{enum.JitterFull, enum.JitterEqual}[v%2],
		Factor:  1.5 + float64(v),
		FirstMs: int64(250 + v),
		MaxMs:   int64(9000 + v),
	})
	s.SetTargets([]string{fmt.Sprintf("agent-%d", v), fmt.Sprintf("agent-%d", v+10)})
	s.SetTargetLabels(map[string]string{"env": fmt.Sprintf("prod-%d", v), "region": fmt.Sprintf("eu-%d", v)})
	s.SetRunnerLabels(map[string]string{"pool": fmt.Sprintf("gpu-%d", v)})
	s.SetVersion(4 + v)
	s.SetGeneration(3 + v)
	s.SetDeletionRequested(v%2 == 0)
	s.SetCreatedAt(created)
	s.SetUpdatedAt(created.Add(time.Duration(v+1) * time.Minute))
	return s
}

// fullSpec is spec id with every field set from v: with v 0, none to its zero or
// default value.
func fullSpec(t *testing.T, id string, v int, created time.Time) *model.Spec {
	t.Helper()
	return newSpec(t, id, fmt.Sprintf("job %s v%d", id, v), fmt.Sprintf("slot-%s-%d", id, v), v, created)
}

// equalSpec compares every accessor of a spec read back with the spec written.
func equalSpec(t *testing.T, what string, got, want *model.Spec) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil spec, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("Name", got.Name(), want.Name())
	f.same("Slot", got.Slot(), want.Slot())
	f.same("Version", got.Version(), want.Version())
	f.same("Generation", got.Generation(), want.Generation())
	f.same("DeletionRequested", got.DeletionRequested(), want.DeletionRequested())
	f.same("KindType", got.KindType(), want.KindType())
	f.same("KindConfig", got.KindConfig(), want.KindConfig())
	f.same("TimeoutMs", got.TimeoutMs(), want.TimeoutMs())
	f.same("RestartType", got.RestartType(), want.RestartType())
	f.same("IntervalMs", got.IntervalMs(), want.IntervalMs())
	f.same("Backoff", got.Backoff(), want.Backoff())
	f.same("Targets", got.Targets(), want.Targets())
	f.same("TargetLabels", got.TargetLabels(), want.TargetLabels())
	f.same("RunnerLabels", got.RunnerLabels(), want.RunnerLabels())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
