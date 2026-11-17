package storagecontract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// workloadSchemas checks the catalog of workload schemas, keyed by fingerprint:
// the round trip, an upsert that records only a new fingerprint, the entries a
// store refuses, the lookup of an unknown fingerprint, the paginated listing
// and a transaction that rolls back.
func workloadSchemas(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := schemaEntry(t, "one", 0, minute(0))
		must(t, s.UpsertWorkloadSchema(ctx, want))

		got, err := s.GetWorkloadSchema(ctx, want.ID())
		gotRow(t, "GetWorkloadSchema", got, err, want, equalWorkloadSchema)
		listed, err := listedWorkloadSchemas(ctx, s)
		must(t, err)
		equalRows(t, "ListWorkloadSchemas", listed, []*model.WorkloadSchema{want}, equalWorkloadSchema)
	})

	t.Run("an unknown fingerprint is not found", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, s.UpsertWorkloadSchema(ctx, schemaEntry(t, "one", 0, minute(0))))
		expect(t,
			outcome{"GetWorkloadSchema of a fingerprint never written", errOf(s.GetWorkloadSchema(ctx, schemaEntry(t, "other", 0, minute(0)).ID())), storage.ErrNotFound},
			outcome{"GetWorkloadSchema of an empty fingerprint", errOf(s.GetWorkloadSchema(ctx, "")), storage.ErrInvalidArgument},
		)
	})

	t.Run("upsert records only a new fingerprint", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		first := schemaEntry(t, "one", 0, minute(0))
		must(t, s.UpsertWorkloadSchema(ctx, first))

		// The same schema offered again, by another agent for another type and
		// later, is no error and changes nothing: the first record stays.
		replay := schemaEntry(t, "one", 1, minute(5))
		if replay.ID() != first.ID() {
			t.Fatalf("the replay has fingerprint %s, want the first record's %s", replay.ID(), first.ID())
		}
		must(t, s.UpsertWorkloadSchema(ctx, replay))
		got, err := s.GetWorkloadSchema(ctx, first.ID())
		gotRow(t, "GetWorkloadSchema after a replay", got, err, first, equalWorkloadSchema)
		listed, err := listedWorkloadSchemas(ctx, s)
		must(t, err)
		equalRows(t, "ListWorkloadSchemas after a replay", listed, []*model.WorkloadSchema{first}, equalWorkloadSchema)
	})

	t.Run("Schema returns a copy of the stored bytes", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := schemaEntry(t, "one", 0, minute(0))
		must(t, s.UpsertWorkloadSchema(ctx, want))
		got, err := s.GetWorkloadSchema(ctx, want.ID())
		must(t, err)
		got.Schema()[0] = 'X'
		again, err := s.GetWorkloadSchema(ctx, want.ID())
		gotRow(t, "GetWorkloadSchema after a change of the copy", again, err, want, equalWorkloadSchema)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// An entry is built by its validating constructor: the zero entry is the
		// one without a fingerprint or a time.
		expect(t,
			outcome{"UpsertWorkloadSchema of nil", s.UpsertWorkloadSchema(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertWorkloadSchema of the zero entry", s.UpsertWorkloadSchema(ctx, &model.WorkloadSchema{}), storage.ErrInvalidArgument},
		)
		listed, err := listedWorkloadSchemas(ctx, s)
		sameIDs(t, "ListWorkloadSchemas after the refused writes", listed, err)
	})

	t.Run("paginated listing", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// Two entries share a time, so the listing breaks the tie by fingerprint.
		var want []*model.WorkloadSchema
		for i, at := range []int{3, 1, 4, 1, 5} {
			want = append(want, schemaEntry(t, fmt.Sprintf("s%d", i), 0, minute(at)))
		}
		for _, w := range want {
			must(t, s.UpsertWorkloadSchema(ctx, w))
		}
		// Newest first, then by fingerprint.
		slices.SortFunc(want, func(a, b *model.WorkloadSchema) int {
			if c := b.FirstSeenAt().Compare(a.FirstSeenAt()); c != 0 {
				return c
			}
			return compareStrings(a.ID(), b.ID())
		})

		var got []*model.WorkloadSchema
		cursor, pages := "", 0
		for {
			page, err := s.ListWorkloadSchemas(ctx, storage.ListOptions{Limit: 2, Cursor: cursor})
			must(t, err)
			if len(page.Items) > 2 {
				t.Fatalf("a page of %d entries, limit 2", len(page.Items))
			}
			got = append(got, page.Items...)
			pages++
			if page.NextCursor == "" {
				break
			}
			if cursor = page.NextCursor; pages > len(want) {
				t.Fatal("the cursor never ends")
			}
		}
		if pages != 3 {
			t.Errorf("%d pages of 5 entries at limit 2, want 3", pages)
		}
		equalRows(t, "the pages of ListWorkloadSchemas", got, want, equalWorkloadSchema)

		empty, err := newStore(t).ListWorkloadSchemas(ctx, storage.ListOptions{Limit: 2})
		must(t, err)
		if len(empty.Items) != 0 || empty.NextCursor != "" {
			t.Errorf("an empty catalog lists %d entries, cursor %q", len(empty.Items), empty.NextCursor)
		}
	})

	t.Run("a transaction commits and rolls back", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept, gone := schemaEntry(t, "kept", 0, minute(0)), schemaEntry(t, "gone", 0, minute(1))
		must(t, s.UpsertWorkloadSchema(ctx, kept))

		err := s.WithTx(ctx, func(tx storage.Storage) error {
			if err := tx.UpsertWorkloadSchema(ctx, gone); err != nil {
				return err
			}
			got, err := tx.GetWorkloadSchema(ctx, gone.ID())
			gotRow(t, "GetWorkloadSchema inside the transaction", got, err, gone, equalWorkloadSchema)
			listed, err := listedWorkloadSchemas(ctx, tx)
			must(t, err)
			equalRows(t, "ListWorkloadSchemas inside the transaction", listed, []*model.WorkloadSchema{gone, kept}, equalWorkloadSchema)
			// A schema the transaction sees is not recorded again.
			return errRollback
		})
		expect(t,
			outcome{"WithTx", err, errRollback},
			outcome{"GetWorkloadSchema of the entry rolled back", errOf(s.GetWorkloadSchema(ctx, gone.ID())), storage.ErrNotFound},
		)
		listed, err := listedWorkloadSchemas(ctx, s)
		must(t, err)
		equalRows(t, "ListWorkloadSchemas after the rollback", listed, []*model.WorkloadSchema{kept}, equalWorkloadSchema)

		// And one that commits, replaying a recorded entry inside it, changes
		// nothing for the replay.
		replay := schemaEntry(t, "kept", 1, minute(9))
		must(t, s.WithTx(ctx, func(tx storage.Storage) error {
			if err := tx.UpsertWorkloadSchema(ctx, replay); err != nil {
				return err
			}
			return tx.UpsertWorkloadSchema(ctx, gone)
		}))
		got, err := s.GetWorkloadSchema(ctx, kept.ID())
		gotRow(t, "GetWorkloadSchema of the replayed entry", got, err, kept, equalWorkloadSchema)
		got, err = s.GetWorkloadSchema(ctx, gone.ID())
		gotRow(t, "GetWorkloadSchema of the committed entry", got, err, gone, equalWorkloadSchema)
	})
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// listedWorkloadSchemas drains the listing of the catalog.
func listedWorkloadSchemas(ctx context.Context, s storage.Storage) ([]*model.WorkloadSchema, error) {
	return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
		return s.ListWorkloadSchemas(ctx, o)
	})
}

// schemaEntry is the entry of the schema named name, first seen at seen, with
// every other field set from v. The fingerprint depends on name alone, so the
// entries of two values of v are one schema.
func schemaEntry(t *testing.T, name string, v int, seen time.Time) *model.WorkloadSchema {
	t.Helper()
	sum := sha256.Sum256([]byte("schema-" + name))
	body := []byte(fmt.Sprintf(`{"type":"object","title":"%s","description":"héllo \u0000 ☃"}`, name))
	w, err := model.NewWorkloadSchema(hex.EncodeToString(sum[:]), fmt.Sprintf("solti.io/v%d", v+1), fmt.Sprintf("Kind%d", v), body, fmt.Sprintf("agent-%d", v), seen)
	if err != nil {
		t.Fatalf("schemaEntry: %v", err)
	}
	return w
}

func equalWorkloadSchema(t *testing.T, what string, got, want *model.WorkloadSchema) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil workload schema, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("APIVersion", got.APIVersion(), want.APIVersion())
	f.same("Kind", got.Kind(), want.Kind())
	f.same("Schema", got.Schema(), want.Schema())
	f.same("FirstAgentID", got.FirstAgentID(), want.FirstAgentID())
	f.instant("FirstSeenAt", got.FirstSeenAt(), want.FirstSeenAt())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
}
