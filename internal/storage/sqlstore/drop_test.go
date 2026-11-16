package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// migratedURL is migrated that also returns the database's URL.
func migratedURL(t *testing.T) (*sql.DB, string) {
	t.Helper()
	databaseURL := pgtest.NewDatabase(t)
	db := pgtest.Open(t, databaseURL)
	if err := Migrate(t.Context(), db, Postgres{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, databaseURL
}

// TestDropSchemaDropsEveryTable drops the schema of a migrated database that
// holds rows, and a table of someone else's: every table of the schema goes,
// with its rows, in the list DropSchema returns, sorted; the other table stays;
// SchemaVersion reads 0 and Open refuses the schema as older, as on an empty
// database; and a migration brings the schema back whole.
func TestDropSchemaDropsEveryTable(t *testing.T) {
	db := migrated(t)
	ctx := t.Context()
	for _, statement := range []string{
		`INSERT INTO podium_events (kind, payload) VALUES ('notify', '\x00')`,
		`CREATE TABLE someone_elses (id int)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	dropped, err := DropSchema(ctx, db, Postgres{}, nil)
	if err != nil {
		t.Fatalf("DropSchema: %v", err)
	}
	if !slices.Equal(dropped, schemaTables) {
		t.Errorf("dropped %q, want %q", dropped, schemaTables)
	}
	if got := tables(t, db); !slices.Equal(got, []string{"someone_elses"}) {
		t.Errorf("tables %q after the drop, want someone_elses alone", got)
	}
	if v, err := SchemaVersion(ctx, db); err != nil || v != 0 {
		t.Errorf("SchemaVersion after the drop = %d, %v; want 0", v, err)
	}
	if s, err := Open(ctx, Options{Primary: db, Dialect: Postgres{}}); s != nil || !errors.Is(err, ErrSchemaOlder) {
		t.Errorf("Open after the drop: %v, %v; want no store and ErrSchemaOlder", s, err)
	}

	if err := Migrate(ctx, db, Postgres{}); err != nil {
		t.Fatalf("migrate after the drop: %v", err)
	}
	if got, want := tables(t, db), append(slices.Clone(schemaTables), "someone_elses"); !slices.Equal(got, want) {
		t.Errorf("tables %q after the migration, want %q", got, want)
	}
}

// TestDropSchemaWithNothingToDrop drops the schema of a database that holds none
// of podium's tables, only someone else's: nothing is dropped, no error, the
// guard is never asked, and the other table stays.
func TestDropSchemaWithNothingToDrop(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE TABLE someone_elses (id int)`); err != nil {
		t.Fatal(err)
	}
	asked := false
	dropped, err := DropSchema(ctx, db, Postgres{}, func(context.Context, *sql.Tx) error {
		asked = true
		return nil
	})
	if dropped != nil || err != nil || asked {
		t.Errorf("DropSchema = %q, %v, the guard asked %v; want nothing dropped, no error, no guard", dropped, err, asked)
	}
	if got := tables(t, db); !slices.Equal(got, []string{"someone_elses"}) {
		t.Errorf("tables %q, want someone_elses alone", got)
	}
}

// TestDropSchemaOfAPartialSchema drops a schema that lacks some of this podium's
// tables: the tables present go, and the list names them alone.
func TestDropSchemaOfAPartialSchema(t *testing.T) {
	db := migrated(t)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `DROP TABLE podium_events, podium_replicas`); err != nil {
		t.Fatal(err)
	}
	want := slices.DeleteFunc(slices.Clone(schemaTables), func(name string) bool {
		return name == "podium_events" || name == "podium_replicas"
	})
	dropped, err := DropSchema(ctx, db, Postgres{}, nil)
	if err != nil || !slices.Equal(dropped, want) {
		t.Errorf("DropSchema = %q, %v; want %q dropped", dropped, err, want)
	}
	if got := tables(t, db); len(got) != 0 {
		t.Errorf("tables %q after the drop, want none", got)
	}
}

// TestDropSchemaRefusesANewerSchema drops the schema of a database a newer
// podium migrated to version 99, whose tables this podium may not know:
// DropSchema refuses with ErrSchemaNewer, naming both versions, without asking
// the guard, and drops nothing.
func TestDropSchemaRefusesANewerSchema(t *testing.T) {
	db := migrated(t)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `INSERT INTO podium_schema_migrations (version) VALUES (99)`); err != nil {
		t.Fatal(err)
	}
	asked := false
	dropped, err := DropSchema(ctx, db, Postgres{}, func(context.Context, *sql.Tx) error {
		asked = true
		return nil
	})
	want := versionsNamed(99, latest(t))
	if dropped != nil || !errors.Is(err, ErrSchemaNewer) || !strings.Contains(fmt.Sprint(err), want) || asked {
		t.Errorf("DropSchema = %q, %v, the guard asked %v; want ErrSchemaNewer naming %q, nothing dropped, no guard", dropped, err, asked, want)
	}
	if got := tables(t, db); !slices.Equal(got, schemaTables) {
		t.Errorf("tables %q after the refusal, want %q", got, schemaTables)
	}
}

// TestDropSchemaAsksTheGuardUnderTheLock drops a schema with a guard that
// refuses: the guard runs in DropSchema's transaction, which it can read
// through, while the transaction holds podium_schema_migrations ACCESS
// EXCLUSIVE, before any table goes; its refusal comes back as it is, and
// nothing is dropped.
func TestDropSchemaAsksTheGuardUnderTheLock(t *testing.T) {
	db, databaseURL := migratedURL(t)
	observer := pgtest.Open(t, databaseURL)
	ctx := t.Context()
	refused := errors.New("a replica still runs")
	var locked bool
	dropped, err := DropSchema(ctx, db, Postgres{}, func(ctx context.Context, tx *sql.Tx) error {
		var replicas int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM podium_replicas`).Scan(&replicas); err != nil {
			return fmt.Errorf("read through the transaction: %w", err)
		}
		if err := observer.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND c.relname = 'podium_schema_migrations' AND l.mode = 'AccessExclusiveLock' AND l.granted)`).Scan(&locked); err != nil {
			return fmt.Errorf("read the locks: %w", err)
		}
		return refused
	})
	if dropped != nil || !errors.Is(err, refused) {
		t.Errorf("DropSchema = %q, %v; want the guard's refusal and nothing dropped", dropped, err)
	}
	if !locked {
		t.Error("the guard ran while podium_schema_migrations was not locked ACCESS EXCLUSIVE")
	}
	if got := tables(t, db); !slices.Equal(got, schemaTables) {
		t.Errorf("tables %q after the refusal, want %q", got, schemaTables)
	}
}

// TestDropSchemaRechecksTheVersionUnderTheLock drops the schema of a database
// whose migrations lock a migrator holds, a newer podium's, which records
// version 99: DropSchema waits for the lock, then reads the version under it,
// refuses with ErrSchemaNewer, and drops nothing.
func TestDropSchemaRechecksTheVersionUnderTheLock(t *testing.T) {
	db, databaseURL := migratedURL(t)
	dropper := pgtest.Open(t, withApplicationName(t, databaseURL, migratorName))
	ctx := t.Context()
	other, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback() }()
	for _, statement := range []string{lockMigrations, `INSERT INTO podium_schema_migrations (version) VALUES (99)`} {
		if _, err := other.ExecContext(ctx, statement); err != nil {
			t.Fatalf("the newer migrator: %v", err)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := DropSchema(ctx, dropper, Postgres{}, nil)
		done <- err
	}()
	waitFor(t, 10*time.Second, "the drop waits on the lock", func() bool { return waitsOnALock(t, db, migratorName) })
	if err := other.Commit(); err != nil {
		t.Fatalf("commit the newer migrator: %v", err)
	}
	want := versionsNamed(99, latest(t))
	if err := <-done; !errors.Is(err, ErrSchemaNewer) || !strings.Contains(fmt.Sprint(err), want) {
		t.Errorf("DropSchema: %v; want ErrSchemaNewer naming %q", err, want)
	}
	if got := tables(t, db); !slices.Equal(got, schemaTables) {
		t.Errorf("tables %q after the refusal, want %q", got, schemaTables)
	}
	if got := versions(t, db); !slices.Equal(got, []int{1, 99}) {
		t.Errorf("recorded versions %v after the refusal, want [1 99]", got)
	}
}
