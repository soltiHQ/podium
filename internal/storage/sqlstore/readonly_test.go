package sqlstore

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// TestReadOnlyUsesReplica gives the store a replica that holds other rows than
// the primary: a listing of a context marked read-only reads the replica, an
// unmarked one reads the primary, and so does a transaction, its context marked
// or not, and a marked listing of a store without a replica.
func TestReadOnlyUsesReplica(t *testing.T) {
	primary := migrated(t)
	createNotes(t, primary, "on the primary")
	replica := pgtest.Fresh(t)
	createNotes(t, replica, "on the replica")
	s := openStore(t, Options{Primary: primary, Replica: replica})
	marked := storage.MarkReadOnly(t.Context())

	listed := func(t *testing.T, ctx context.Context, s *Store) []string {
		t.Helper()
		var ids []string
		err := s.listing(ctx, func(db *sql.DB) error {
			var err error
			ids, err = notes(ctx, db)
			return err
		})
		if err != nil {
			t.Fatalf("list the notes: %v", err)
		}
		return ids
	}
	for _, tc := range []struct {
		name  string
		store *Store
		ctx   context.Context
		want  string
	}{
		{"a marked listing", s, marked, "on the replica"},
		{"an unmarked listing", s, t.Context(), "on the primary"},
		{"a marked listing without a replica", openStore(t, Options{Primary: primary}), marked, "on the primary"},
	} {
		if got := listed(t, tc.ctx, tc.store); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s: notes %q, want %q", tc.name, got, tc.want)
		}
	}

	err := s.transact(marked, func(h *handle) error {
		wantNotes(t, "a transaction of a marked context", h.tx, "on the primary")
		return nil
	})
	if err != nil {
		t.Fatalf("transact: %v", err)
	}
}
