package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// querier is what the tests read and write a table through: a pool or a
// transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// migrated returns a pool on a fresh database migrated to this podium's schema.
func migrated(t *testing.T) *sql.DB {
	t.Helper()
	db := pgtest.Fresh(t)
	if err := Migrate(t.Context(), db, Postgres{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// openStore opens a store with opts, the Postgres dialect when opts names none.
func openStore(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.Dialect == nil {
		opts.Dialect = Postgres{}
	}
	s, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return s
}

// The store has no entity yet: the tests of its transactions and of its reads
// use notes, a table of their own, as the entities' methods will use theirs.

// createNotes creates the notes table in db and writes the notes ids.
func createNotes(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE notes (id bytea PRIMARY KEY)`); err != nil {
		t.Fatalf("create the notes: %v", err)
	}
	for _, id := range ids {
		if err := addNote(t.Context(), db, id); err != nil {
			t.Fatalf("add note %q: %v", id, err)
		}
	}
}

// addNote writes the note id, which a bytea parameter takes as a []byte.
func addNote(ctx context.Context, q querier, id string) error {
	_, err := q.ExecContext(ctx, Postgres{}.Rebind(`INSERT INTO notes (id) VALUES (?)`), []byte(id))
	return err
}

// notes returns the IDs of every note, in byte order.
func notes(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM notes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, string(id))
	}
	return ids, rows.Err()
}

// wantNotes fails t unless q holds exactly the notes want.
func wantNotes(t *testing.T, what string, q querier, want ...string) {
	t.Helper()
	got, err := notes(t.Context(), q)
	if err != nil {
		t.Errorf("%s: read the notes: %v", what, err)
		return
	}
	if sorted := slices.Sorted(slices.Values(want)); !slices.Equal(got, sorted) {
		t.Errorf("%s: notes %q, want %q", what, got, sorted)
	}
}

// TestOpenNeedsAPrimaryAndADialect checks that Open refuses options without a
// primary database or a dialect, before it reads anything.
func TestOpenNeedsAPrimaryAndADialect(t *testing.T) {
	// Nothing listens on port 1; Open must refuse before it connects.
	db, err := sql.Open("pgx", "postgres://podium@127.0.0.1:1/podium")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"no options", Options{}},
		{"no primary", Options{Dialect: Postgres{}}},
		{"no dialect", Options{Primary: db}},
	} {
		if s, err := Open(t.Context(), tc.opts); s != nil || !errors.Is(err, storage.ErrInvalidArgument) {
			t.Errorf("Open with %s: %v, %v; want no store and storage.ErrInvalidArgument", tc.name, s, err)
		}
	}
}
