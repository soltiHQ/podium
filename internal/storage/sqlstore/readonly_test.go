package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

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

// TestReplicaDownFallsBackToPrimary (Review Focus 3) gives the store a replica
// that holds other rows than the primary, then takes the replica down: every
// marked listing, scan and count reads the replica while it is up, and the
// primary once it is down, rather than fail. A replica that answers with
// another error, as one without podium's schema, fails the listing: it is no
// outage. So does a marked context that ends while the replica reads, with its
// own error.
func TestReplicaDownFallsBackToPrimary(t *testing.T) {
	primary := migrated(t)
	replicaURL := pgtest.NewDatabase(t)
	replica := pgtest.Open(t, replicaURL)
	if err := Migrate(t.Context(), replica, Postgres{}); err != nil {
		t.Fatalf("migrate the replica: %v", err)
	}
	withReplica := openStore(t, Options{Primary: primary, Replica: replica})
	onPrimary := seed(t, withReplica, "primary", 1)
	onReplica := seed(t, openStore(t, Options{Primary: replica}), "replica", 2)
	marked := storage.MarkReadOnly(t.Context())

	checkReads(t, marked, "the replica up", withReplica, onReplica)
	takeDown(t, primary, replicaURL)
	if err := (Postgres{}).Classify(replica.PingContext(t.Context())); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("the replica taken down answers its ping with %v, want storage.ErrUnavailable", err)
	}
	checkReads(t, marked, "the replica down", withReplica, onPrimary)

	// A replica that answers with another error fails the listing.
	broken := openStore(t, Options{Primary: primary, Replica: pgtest.Fresh(t)})
	if _, err := broken.ListAgents(marked, nil, storage.ListOptions{}); !errors.Is(err, storage.ErrInternal) {
		t.Errorf("a marked listing on a replica without the schema: %v, want its storage.ErrInternal", err)
	}

	// A marked context that ends while the replica reads gets its own error: the
	// replica's table is locked, so its read waits.
	slow := migrated(t)
	lock, err := slow.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Rollback() })
	if _, err := lock.ExecContext(t.Context(), `LOCK TABLE podium_agents IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock the slow replica's agents: %v", err)
	}
	ctx, cancel := context.WithTimeout(marked, 300*time.Millisecond)
	defer cancel()
	waiting := openStore(t, Options{Primary: primary, Replica: slow})
	if _, err := waiting.ListAgents(ctx, nil, storage.ListOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a marked listing whose context ends while the replica reads: %v, want context.DeadlineExceeded", err)
	}
}

// takeDown takes the database at databaseURL down, from admin, a pool on
// another database of the server: it takes no new connection, and the open ones
// end.
func takeDown(t *testing.T, admin *sql.DB, databaseURL string) {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if _, err := admin.ExecContext(t.Context(), `ALTER DATABASE `+name+` WITH ALLOW_CONNECTIONS false`); err != nil {
		t.Fatalf("refuse connections to the replica: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity WHERE datname = $1`, name); err != nil {
		t.Fatalf("end the replica's sessions: %v", err)
	}
}
