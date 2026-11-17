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

// lockedAccessExclusive reports, through observer, whether a session holds the
// table name of observer's database ACCESS EXCLUSIVE.
func lockedAccessExclusive(ctx context.Context, observer *sql.DB, name string) (bool, error) {
	var locked bool
	err := observer.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		WHERE l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND c.relname = $1 AND l.mode = 'AccessExclusiveLock' AND l.granted)`, name).Scan(&locked)
	return locked, err
}

// TestDropSchemaDropsEveryTable drops the schema of a migrated database that
// holds rows, and a table of someone else's: every table of the schema goes,
// with its rows, in the list DropSchema returns, sorted, with the version it
// read; the other table stays; SchemaVersion reads 0 and Open refuses the schema
// as older, as on an empty database; and a migration brings the schema back
// whole.
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
	if !slices.Equal(dropped.Tables, schemaTables) || dropped.Version != latest(t) {
		t.Errorf("dropped %q at version %d, want %q at version %d", dropped.Tables, dropped.Version, schemaTables, latest(t))
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

// TestDropSchemaWithNothingToDrop drops the schema of a database that holds
// none of podium's tables, only someone else's table, and a view with a name of
// podium's, which is no table: nothing is dropped, no error, the guard is never
// asked, and the table and the view stay.
func TestDropSchemaWithNothingToDrop(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	for _, statement := range []string{`CREATE TABLE someone_elses (id int)`, `CREATE VIEW podium_agents AS SELECT 1 AS id`} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	asked := false
	dropped, err := DropSchema(ctx, db, Postgres{}, func(context.Context, *sql.Tx) error {
		asked = true
		return nil
	})
	if dropped.Tables != nil || err != nil || asked {
		t.Errorf("DropSchema = %q, %v, the guard asked %v; want nothing dropped, no error, no guard", dropped.Tables, err, asked)
	}
	if got := tables(t, db); !slices.Equal(got, []string{"podium_agents", "someone_elses"}) {
		t.Errorf("tables and views %q, want podium_agents, the view, and someone_elses", got)
	}
}

// TestDropSchemaRefusesTablesWithoutTheMarker drops the schema of a database
// that holds a table with a name of podium's but no podium_schema_migrations:
// not a schema podium migrated, which DropSchema refuses with
// ErrNotPodiumSchema, naming the table, without asking the guard, and nothing is
// dropped.
func TestDropSchemaRefusesTablesWithoutTheMarker(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE TABLE podium_agents (id int)`); err != nil {
		t.Fatal(err)
	}
	asked := false
	dropped, err := DropSchema(ctx, db, Postgres{}, func(context.Context, *sql.Tx) error {
		asked = true
		return nil
	})
	if dropped.Tables != nil || !errors.Is(err, ErrNotPodiumSchema) || !strings.Contains(fmt.Sprint(err), "podium_agents") || asked {
		t.Errorf("DropSchema = %q, %v, the guard asked %v; want ErrNotPodiumSchema naming podium_agents, nothing dropped, no guard", dropped.Tables, err, asked)
	}
	if got := tables(t, db); !slices.Equal(got, []string{"podium_agents"}) {
		t.Errorf("tables %q after the refusal, want podium_agents", got)
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
	if err != nil || !slices.Equal(dropped.Tables, want) {
		t.Errorf("DropSchema = %q, %v; want %q dropped", dropped.Tables, err, want)
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
	if dropped.Tables != nil || !errors.Is(err, ErrSchemaNewer) || !strings.Contains(fmt.Sprint(err), want) || asked {
		t.Errorf("DropSchema = %q, %v, the guard asked %v; want ErrSchemaNewer naming %q, nothing dropped, no guard", dropped.Tables, err, asked, want)
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
		var err error
		if locked, err = lockedAccessExclusive(ctx, observer, "podium_schema_migrations"); err != nil {
			return fmt.Errorf("read the locks: %w", err)
		}
		return refused
	})
	if dropped.Tables != nil || !errors.Is(err, refused) {
		t.Errorf("DropSchema = %q, %v; want the guard's refusal and nothing dropped", dropped.Tables, err)
	}
	if !locked {
		t.Error("the guard ran while podium_schema_migrations was not locked ACCESS EXCLUSIVE")
	}
	if got := tables(t, db); !slices.Equal(got, schemaTables) {
		t.Errorf("tables %q after the refusal, want %q", got, schemaTables)
	}
}

// TestDropSchemaChecksAgainUnderTheTableLocks drops a schema with a guard that
// lets it: DropSchema asks the guard twice, first with the tables unlocked, so
// that a deployment whose replicas run never queues behind the drop, then with
// every table of the schema locked ACCESS EXCLUSIVE, so that no heartbeat, take
// of the lease or write lands between the second answer and the drop.
func TestDropSchemaChecksAgainUnderTheTableLocks(t *testing.T) {
	db, databaseURL := migratedURL(t)
	observer := pgtest.Open(t, databaseURL)
	ctx := t.Context()
	var locks []bool
	dropped, err := DropSchema(ctx, db, Postgres{}, func(ctx context.Context, _ *sql.Tx) error {
		locked, err := lockedAccessExclusive(ctx, observer, "podium_replicas")
		locks = append(locks, locked)
		return err
	})
	if err != nil || !slices.Equal(dropped.Tables, schemaTables) {
		t.Fatalf("DropSchema = %q, %v; want every table dropped", dropped.Tables, err)
	}
	if !slices.Equal(locks, []bool{false, true}) {
		t.Errorf("podium_replicas locked ACCESS EXCLUSIVE at each guard: %v, want [false true]", locks)
	}
}

// TestDropSchemaGivesUpOnALock drops the schema of a database where another
// session holds a lock on one of its tables, as pg_dump does: DropSchema waits
// for the lock no longer than its lock timeout, then answers ErrLocked, and
// nothing is dropped; once the lock is free, it drops.
func TestDropSchemaGivesUpOnALock(t *testing.T) {
	defer func(d time.Duration) { dropLockTimeout = d }(dropLockTimeout)
	dropLockTimeout = 300 * time.Millisecond
	db, databaseURL := migratedURL(t)
	holder := pgtest.Open(t, databaseURL)
	ctx := t.Context()
	other, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback() }()
	if _, err := other.ExecContext(ctx, `LOCK TABLE podium_agents IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}

	begin := time.Now()
	dropped, err := DropSchema(ctx, db, Postgres{}, nil)
	if dropped.Tables != nil || !errors.Is(err, ErrLocked) {
		t.Fatalf("DropSchema = %q, %v; want ErrLocked and nothing dropped", dropped.Tables, err)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("the refusal took %s, want about the lock timeout of %s", took, dropLockTimeout)
	}
	if got := tables(t, db); !slices.Equal(got, schemaTables) {
		t.Errorf("tables %q after the refusal, want %q", got, schemaTables)
	}

	if err := other.Rollback(); err != nil {
		t.Fatal(err)
	}
	if dropped, err := DropSchema(ctx, db, Postgres{}, nil); err != nil || !slices.Equal(dropped.Tables, schemaTables) {
		t.Errorf("DropSchema once the lock is free = %q, %v; want every table dropped", dropped.Tables, err)
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
	waitFor(t, 4*time.Second, "the drop waits on the lock", func() bool { return waitsOnALock(t, db, migratorName) })
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
	if got := versions(t, db); !slices.Equal(got, []int{1, 2, 99}) {
		t.Errorf("recorded versions %v after the refusal, want [1 2 99]", got)
	}
}
