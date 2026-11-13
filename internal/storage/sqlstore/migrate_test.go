package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
)

// schemaTables are the tables of this podium's schema, sorted.
var schemaTables = []string{
	"podium_agent_credentials",
	"podium_agents",
	"podium_credentials",
	"podium_events",
	"podium_executions",
	"podium_leader",
	"podium_replicas",
	"podium_roles",
	"podium_rollouts",
	"podium_schema_migrations",
	"podium_sessions",
	"podium_specs",
	"podium_users",
	"podium_verifiers",
}

// entityTables are the tables that hold the entities, one per entity group.
var entityTables = []string{
	"podium_agent_credentials",
	"podium_agents",
	"podium_credentials",
	"podium_executions",
	"podium_roles",
	"podium_rollouts",
	"podium_sessions",
	"podium_specs",
	"podium_users",
	"podium_verifiers",
}

// latest is the version of the newest migration of the Postgres dialect.
func latest(t *testing.T) int {
	t.Helper()
	files, err := migrationFiles(Postgres{}.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

// column returns the first column of each row query returns, in order, scanned
// into T; what names the read in a failure.
func column[T any](t *testing.T, db *sql.DB, what, query string) []T {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	defer func() { _ = rows.Close() }()
	values := []T{}
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return values
}

// tables returns the tables of db's current schema, sorted.
func tables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return column[string](t, db, "list the tables",
		`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() ORDER BY table_name`)
}

// versions returns the versions podium_schema_migrations records, in order.
func versions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	return column[int](t, db, "read the recorded versions", `SELECT version FROM podium_schema_migrations ORDER BY version`)
}

// versionsNamed is how a refusal names the two versions.
func versionsNamed(database, podium int) string {
	return fmt.Sprintf("the database is at version %d, this podium at version %d", database, podium)
}

// TestMigrateCreatesSchema migrates an empty database: every table of the
// schema, version 1 recorded, and the column types the store relies on.
func TestMigrateCreatesSchema(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	if err := Migrate(ctx, db, Postgres{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := tables(t, db); !slices.Equal(got, schemaTables) {
		t.Errorf("tables %q, want %q", got, schemaTables)
	}
	if v, err := SchemaVersion(ctx, db); err != nil || v != 1 || latest(t) != 1 {
		t.Errorf("SchemaVersion = %d, %v, the dialect's latest %d; want version 1", v, err, latest(t))
	}
	if got := versions(t, db); !slices.Equal(got, []int{1}) {
		t.Errorf("recorded versions %v, want [1]", got)
	}

	// An entity's strings and its body are bytea, which compares by bytes and
	// carries a NUL, and its times bigint nanoseconds; a status podium fills from
	// its own enum may be a smallint, and the execution record's text.
	rows, err := db.QueryContext(ctx, `SELECT table_name, column_name, data_type FROM information_schema.columns
		WHERE table_schema = current_schema() ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatalf("list the columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	allowed := map[string]bool{"bytea": true, "bigint": true, "smallint": true}
	for rows.Next() {
		var table, column, typ string
		if err := rows.Scan(&table, &column, &typ); err != nil {
			t.Fatalf("list the columns: %v", err)
		}
		if !slices.Contains(entityTables, table) {
			continue
		}
		want := ""
		switch column {
		case "id", "body":
			want = "bytea"
		case "created_at":
			want = "bigint"
		}
		switch {
		case want != "" && typ != want:
			t.Errorf("%s.%s is %s, want %s", table, column, typ, want)
		case table == "podium_executions" && column == "status":
			if typ != "text" {
				t.Errorf("%s.%s is %s, want text", table, column, typ)
			}
		case !allowed[typ]:
			t.Errorf("%s.%s is %s, want bytea, bigint or smallint", table, column, typ)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the columns: %v", err)
	}
}

// TestMigrateIsIdempotent migrates a database again and again: one schema,
// version 1 recorded once.
func TestMigrateIsIdempotent(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	for i := range 3 {
		// A new pool each time, as a replica that starts again opens one.
		if err := Migrate(t.Context(), pgtest.Open(t, databaseURL), Postgres{}); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	db := pgtest.Open(t, databaseURL)
	if got := tables(t, db); !slices.Equal(got, schemaTables) {
		t.Errorf("tables %q, want %q", got, schemaTables)
	}
	if got := versions(t, db); !slices.Equal(got, []int{1}) {
		t.Errorf("recorded versions %v, want [1]", got)
	}
	if v, err := SchemaVersion(t.Context(), db); err != nil || v != 1 {
		t.Errorf("SchemaVersion = %d, %v; want 1", v, err)
	}
}

// TestNewerSchemaRefused opens and migrates a database a newer podium migrated
// to version 99, as after a rollback: both refuse with ErrSchemaNewer, naming
// both versions, and write nothing.
func TestNewerSchemaRefused(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	for _, statement := range []string{createMigrationsTable, `INSERT INTO podium_schema_migrations (version) VALUES (99)`} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("record version 99: %v", err)
		}
	}
	want := versionsNamed(99, latest(t))

	s, err := Open(ctx, Options{Primary: db, Dialect: Postgres{}})
	if s != nil || !errors.Is(err, ErrSchemaNewer) || !strings.Contains(fmt.Sprint(err), want) {
		t.Errorf("Open: %v, %v; want no store and ErrSchemaNewer naming %q", s, err, want)
	}
	if err := Migrate(ctx, db, Postgres{}); !errors.Is(err, ErrSchemaNewer) || !strings.Contains(fmt.Sprint(err), want) {
		t.Errorf("Migrate: %v; want ErrSchemaNewer naming %q", err, want)
	}

	if got := tables(t, db); !slices.Equal(got, []string{"podium_schema_migrations"}) {
		t.Errorf("tables %q after the refusals, want only podium_schema_migrations", got)
	}
	if got := versions(t, db); !slices.Equal(got, []int{99}) {
		t.Errorf("recorded versions %v after the refusals, want [99]", got)
	}
}

// TestOlderSchemaRefused opens an empty database, whose schema is older than
// this podium's: Open refuses with ErrSchemaOlder, naming both versions, and
// writes nothing; once migrated, it opens.
func TestOlderSchemaRefused(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	want := versionsNamed(0, latest(t))

	s, err := Open(ctx, Options{Primary: db, Dialect: Postgres{}})
	if s != nil || !errors.Is(err, ErrSchemaOlder) || !strings.Contains(fmt.Sprint(err), want) {
		t.Errorf("Open: %v, %v; want no store and ErrSchemaOlder naming %q", s, err, want)
	}
	if !strings.Contains(fmt.Sprint(err), "run podium migrate") {
		t.Errorf("Open: %v; want it to say: run podium migrate", err)
	}
	if got := tables(t, db); len(got) != 0 {
		t.Errorf("tables %q after the refusal, want none", got)
	}

	if err := Migrate(ctx, db, Postgres{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := Open(ctx, Options{Primary: db, Dialect: Postgres{}}); err != nil {
		t.Errorf("Open after the migration: %v", err)
	}
}

// TestConcurrentMigrate migrates one empty database from two pools at once, as
// two replicas that start together do: no error, one schema. Two sessions that
// create one table at once rarely meet, so a second check makes the meeting
// happen: the migrator creates podium_schema_migrations while another session's
// creation of it waits to commit, and Postgres fails the migrator's once the
// other commits.
func TestConcurrentMigrate(t *testing.T) {
	t.Run("two migrators", func(t *testing.T) {
		databaseURL := pgtest.NewDatabase(t)
		pools := []*sql.DB{pgtest.Open(t, databaseURL), pgtest.Open(t, databaseURL)}
		start := make(chan struct{})
		errs := make(chan error, len(pools))
		for _, db := range pools {
			go func() {
				<-start
				errs <- Migrate(t.Context(), db, Postgres{})
			}()
		}
		close(start)
		for range pools {
			if err := <-errs; err != nil {
				t.Errorf("migrate: %v", err)
			}
		}
		if got := tables(t, pools[0]); !slices.Equal(got, schemaTables) {
			t.Errorf("tables %q, want %q", got, schemaTables)
		}
		if got := versions(t, pools[0]); !slices.Equal(got, []int{1}) {
			t.Errorf("recorded versions %v, want [1]", got)
		}
	})

	t.Run("a creation of podium_schema_migrations that commits first", func(t *testing.T) {
		db := pgtest.Fresh(t)
		ctx := t.Context()
		other, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = other.Rollback() }()
		if _, err := other.ExecContext(ctx, createMigrationsTable); err != nil {
			t.Fatalf("create podium_schema_migrations in the other session: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- Migrate(ctx, db, Postgres{}) }()
		// The migrator's creation waits on the other session's, uncommitted.
		waitFor(t, 10*time.Second, "the migrator waits on a lock", func() bool { return waitsOnALock(t, db) })
		if err := other.Commit(); err != nil {
			t.Fatalf("commit the other session: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("migrate: %v", err)
		}
		if got := tables(t, db); !slices.Equal(got, schemaTables) {
			t.Errorf("tables %q, want %q", got, schemaTables)
		}
		if got := versions(t, db); !slices.Equal(got, []int{1}) {
			t.Errorf("recorded versions %v, want [1]", got)
		}
	})
}

// waitsOnALock reports whether a session of db's database waits on a lock.
func waitsOnALock(t *testing.T, db *sql.DB) bool {
	var waiting int
	err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
	return err == nil && waiting > 0
}

// fsDialect is the Postgres dialect over migrations a test writes.
type fsDialect struct {
	Postgres
	files fs.FS
}

func (d fsDialect) Migrations() fs.FS { return d.files }

// TestMigrationsRunOneToATransaction migrates with a first file that applies and
// a second that fails halfway: the second rolls back whole, the first stays
// applied and recorded, and Open still refuses the schema as older.
func TestMigrationsRunOneToATransaction(t *testing.T) {
	d := fsDialect{files: fstest.MapFS{
		"0001_first.sql": {Data: []byte(`CREATE TABLE first_table (id int);`)},
		// Its first statement succeeds, its second fails.
		"0002_second.sql": {Data: []byte(`CREATE TABLE second_table (id int); INSERT INTO no_such_table VALUES (1);`)},
	}}
	db := pgtest.Fresh(t)
	ctx := t.Context()

	err := Migrate(ctx, db, d)
	if err == nil || !strings.Contains(err.Error(), "0002_second.sql") || strings.Contains(err.Error(), "0001_first.sql") {
		t.Fatalf("Migrate: %v, want the failure of 0002_second.sql alone", err)
	}
	if want := []string{"first_table", "podium_schema_migrations"}; !slices.Equal(tables(t, db), want) {
		t.Errorf("tables %q, want %q: the first migration kept, the second rolled back whole", tables(t, db), want)
	}
	if got := versions(t, db); !slices.Equal(got, []int{1}) {
		t.Errorf("recorded versions %v, want [1]", got)
	}
	if v, err := SchemaVersion(ctx, db); err != nil || v != 1 {
		t.Errorf("SchemaVersion = %d, %v; want 1", v, err)
	}
	if s, err := Open(ctx, Options{Primary: db, Dialect: d}); s != nil || !errors.Is(err, ErrSchemaOlder) {
		t.Errorf("Open: %v, %v; want no store and ErrSchemaOlder", s, err)
	}
}

// TestMigrateRechecksTheVersionUnderTheLock starts a migration of a database at
// version 0 whose lock another migrator holds, a newer podium's, which records
// version 99: once the lock is free the migration reads that version again and
// refuses with ErrSchemaNewer, applying nothing.
func TestMigrateRechecksTheVersionUnderTheLock(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		t.Fatal(err)
	}
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
	go func() { done <- Migrate(ctx, db, Postgres{}) }()
	// The migration read version 0, and waits for the lock.
	waitFor(t, 10*time.Second, "the migrator waits on the lock", func() bool { return waitsOnALock(t, db) })
	if err := other.Commit(); err != nil {
		t.Fatalf("commit the newer migrator: %v", err)
	}
	want := versionsNamed(99, latest(t))
	if err := <-done; !errors.Is(err, ErrSchemaNewer) || !strings.Contains(err.Error(), want) {
		t.Errorf("Migrate: %v; want ErrSchemaNewer naming %q", err, want)
	}
	if got := tables(t, db); !slices.Equal(got, []string{"podium_schema_migrations"}) {
		t.Errorf("tables %q after the refusal, want only podium_schema_migrations", got)
	}
	if got := versions(t, db); !slices.Equal(got, []int{99}) {
		t.Errorf("recorded versions %v after the refusal, want [99]", got)
	}
}

// waitFor polls cond every 10 ms until it holds, and fails t when it still does
// not after timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, timeout)
		}
		<-tick.C
	}
}

// TestMigrationFiles checks how the migrator reads a dialect's files: named
// NNNN_name.sql, versions 1, 2, 3… without a gap or a repeat.
func TestMigrationFiles(t *testing.T) {
	files, err := migrationFiles(Postgres{}.Migrations())
	if err != nil || len(files) != 1 || files[0].version != 1 || files[0].name != "0001_init.sql" {
		t.Fatalf("the Postgres migrations: %+v, %v; want 0001_init.sql alone", files, err)
	}
	// The migrator creates podium_schema_migrations before the first migration,
	// as it must lock the table; the first migration repeats the statement, so
	// that the file holds the whole schema.
	if !strings.Contains(files[0].sql, createMigrationsTable) {
		t.Errorf("0001_init.sql does not repeat the migrator's statement:\n%s", createMigrationsTable)
	}

	file := &fstest.MapFile{Data: []byte("SELECT 1;")}
	for _, tc := range []struct {
		name string
		fsys fstest.MapFS
	}{
		{"no file", fstest.MapFS{}},
		{"a gap", fstest.MapFS{"0001_init.sql": file, "0003_more.sql": file}},
		{"a repeated version", fstest.MapFS{"0001_init.sql": file, "0001_again.sql": file}},
		{"no version 1", fstest.MapFS{"0002_more.sql": file}},
		{"a name without a version", fstest.MapFS{"0001_init.sql": file, "init.sql": file}},
	} {
		if files, err := migrationFiles(tc.fsys); err == nil {
			t.Errorf("migration files with %s: %+v, want an error", tc.name, files)
		}
	}
}
