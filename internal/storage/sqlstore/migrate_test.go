package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
	"github.com/soltiHQ/control-plane/migrations"
)

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
		databaseURL := pgtest.NewDatabase(t)
		db := pgtest.Open(t, databaseURL)
		migrator := pgtest.Open(t, withApplicationName(t, databaseURL, migratorName))
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
		go func() { done <- Migrate(ctx, migrator, Postgres{}) }()
		// The migrator's creation waits on the other session's, uncommitted.
		waitFor(t, 10*time.Second, "the migrator waits on a lock", func() bool { return waitsOnALock(t, db, migratorName) })
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

// migratorName is the application_name of the migrator's sessions in the tests
// that wait for it to wait on a lock.
const migratorName = "podium-test-migrator"

// withApplicationName is databaseURL with application_name name, which names
// the sessions of a pool on it in pg_stat_activity.
func withApplicationName(t *testing.T, databaseURL, name string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("the database URL does not parse")
	}
	q := u.Query()
	q.Set("application_name", name)
	u.RawQuery = q.Encode()
	return u.String()
}

// waitsOnALock reports whether a session of db's database whose
// application_name is name, the migrator's, waits on a lock: the test's own
// sessions, which hold the lock, are not looked at. A query that fails fails the
// test at once, instead of passing for no wait until the deadline.
func waitsOnALock(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var waiting bool
	err := db.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = $1 AND wait_event_type = 'Lock')`, name).Scan(&waiting)
	if err != nil {
		t.Fatalf("read the lock waits of the migrator: %v", err)
	}
	return waiting
}

// fsDialect is the Postgres dialect over migrations a test writes.
type fsDialect struct {
	Postgres
	files fs.FS
}

func (d fsDialect) Migrations() fs.FS { return d.files }

// stamped is migration file version: statements, then the statement that
// records the version, as every migration file ends.
func stamped(version int, statements string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(statements + "\n" + fmt.Sprintf(stampMigration, version) + "\n")}
}

// TestMigrationsRunOneToATransaction migrates with a first file that applies and
// a second that fails halfway: the second rolls back whole, the first stays
// applied and recorded, and Open still refuses the schema as older.
func TestMigrationsRunOneToATransaction(t *testing.T) {
	d := fsDialect{files: fstest.MapFS{
		"0001_first.sql": stamped(1, `CREATE TABLE first_table (id int);`),
		// Its first statement succeeds, its second fails.
		"0002_second.sql": stamped(2, `CREATE TABLE second_table (id int); INSERT INTO no_such_table VALUES (1);`),
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

// TestMigrationMustLeaveItsVersion migrates with a second file that ends with
// the statement that records its version, but moves the version recorded
// before it past its own: the migrator finds the schema at another version
// than the file's once it ran, and rolls the file back.
func TestMigrationMustLeaveItsVersion(t *testing.T) {
	d := fsDialect{files: fstest.MapFS{
		"0001_first.sql":  stamped(1, `CREATE TABLE first_table (id int);`),
		"0002_second.sql": stamped(2, `CREATE TABLE second_table (id int); UPDATE podium_schema_migrations SET version = 9;`),
	}}
	db := pgtest.Fresh(t)

	err := Migrate(t.Context(), db, d)
	if want := "migration 0002_second.sql left the schema at version 9, want 2"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Migrate: %v, want an error naming %q", err, want)
	}
	if want := []string{"first_table", "podium_schema_migrations"}; !slices.Equal(tables(t, db), want) {
		t.Errorf("tables %q, want %q: the second migration rolled back", tables(t, db), want)
	}
	if got := versions(t, db); !slices.Equal(got, []int{1}) {
		t.Errorf("recorded versions %v, want [1]", got)
	}
}

// TestSchemaAppliedByAnotherTool applies the Postgres migrations as a tool of
// the deployment's own does, as Bytebase: each file as it is, in a transaction
// of its own, in version order, without podium's migrator. The files record
// their versions themselves, so the schema is at podium's version: Open takes
// it, and Migrate finds nothing to apply.
func TestSchemaAppliedByAnotherTool(t *testing.T) {
	db := pgtest.Fresh(t)
	ctx := t.Context()
	names, err := fs.Glob(migrations.Postgres(), "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("the migration files: %q, %v", names, err)
	}
	want := []int{}
	for i, name := range names {
		body, err := fs.ReadFile(migrations.Postgres(), name)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
		want = append(want, i+1)
	}
	if got := versions(t, db); !slices.Equal(got, want) {
		t.Fatalf("recorded versions %v, want %v: each file records its own", got, want)
	}
	if _, err := Open(ctx, Options{Primary: db, Dialect: Postgres{}}); err != nil {
		t.Errorf("Open: %v, want the schema taken", err)
	}
	if err := Migrate(ctx, db, Postgres{}); err != nil {
		t.Errorf("Migrate: %v, want nothing to apply", err)
	}
	if got := versions(t, db); !slices.Equal(got, want) {
		t.Errorf("recorded versions %v after Migrate, want %v", got, want)
	}
	if got := tables(t, db); !slices.Equal(got, schemaTables) {
		t.Errorf("tables %q, want %q", got, schemaTables)
	}
}

// TestMigrateRechecksTheVersionUnderTheLock starts a migration of a database at
// version 0 whose lock another migrator holds, a newer podium's, which records
// version 99: once the lock is free the migration reads that version again and
// refuses with ErrSchemaNewer, applying nothing.
//
// The migration reads version 0 first, before it waits: Migrate's first read of
// the version runs outside the lock, a plain read that the other migrator's
// EXCLUSIVE lock lets through, and the other's version 99 is uncommitted then.
// So the migration goes on to take the lock, and waits there; only its read
// under the lock, once the other committed, can see 99. That second read is
// what the test pins.
func TestMigrateRechecksTheVersionUnderTheLock(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	db := pgtest.Open(t, databaseURL)
	migrator := pgtest.Open(t, withApplicationName(t, databaseURL, migratorName))
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
	go func() { done <- Migrate(ctx, migrator, Postgres{}) }()
	// The migration read version 0, and waits for the lock.
	waitFor(t, 10*time.Second, "the migrator waits on the lock", func() bool { return waitsOnALock(t, db, migratorName) })
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

// TestLatestVersion checks LatestVersion: the version of a dialect's newest
// migration, its files checked as the migrator checks them.
func TestLatestVersion(t *testing.T) {
	if v, err := LatestVersion(Postgres{}); err != nil || v != latest(t) {
		t.Errorf("LatestVersion(Postgres) = %d, %v; want %d", v, err, latest(t))
	}
	two := fsDialect{files: fstest.MapFS{"0001_first.sql": stamped(1, "SELECT 1;"), "0002_second.sql": stamped(2, "SELECT 1;")}}
	if v, err := LatestVersion(two); err != nil || v != 2 {
		t.Errorf("LatestVersion of two files = %d, %v; want 2", v, err)
	}
	unstamped := fsDialect{files: fstest.MapFS{"0001_first.sql": {Data: []byte("SELECT 1;")}}}
	if v, err := LatestVersion(unstamped); err == nil {
		t.Errorf("LatestVersion of a file without its stamp = %d, want an error", v)
	}
}

// TestMigrationFiles checks how the migrator reads a dialect's files: named
// NNNN_name.sql, versions 1, 2, 3… without a gap or a repeat, each ending with
// the statement that records its version, the only one that inserts into
// podium_schema_migrations.
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

	// The statement that records the version may be followed by blank lines
	// and comments, and nothing else.
	trailing := &fstest.MapFile{Data: []byte("SELECT 1;\n" + fmt.Sprintf(stampMigration, 1) + "\n\n-- the end\n")}
	if files, err := migrationFiles(fstest.MapFS{"0001_init.sql": trailing}); err != nil || len(files) != 1 {
		t.Errorf("a file whose stamp blank lines and a comment follow: %+v, %v; want it read", files, err)
	}

	first := stamped(1, "SELECT 1;")
	for _, tc := range []struct {
		name, want string
		fsys       fstest.MapFS
	}{
		{"no file", "has no migrations", fstest.MapFS{}},
		{"a gap", `"0003_more.sql" has version 3, want 2`, fstest.MapFS{"0001_init.sql": first, "0003_more.sql": stamped(3, "SELECT 1;")}},
		{"a repeated version", `"0001_init.sql" has version 1, want 2`, fstest.MapFS{"0001_again.sql": first, "0001_init.sql": first}},
		{"no version 1", `"0002_more.sql" has version 2, want 1`, fstest.MapFS{"0002_more.sql": stamped(2, "SELECT 1;")}},
		{"a name without a version", `"init.sql" is not a file named NNNN_name.sql`, fstest.MapFS{"0001_init.sql": first, "init.sql": first}},
		{"no stamp", `"0001_init.sql" does not end with the statement that records its version`, fstest.MapFS{"0001_init.sql": {Data: []byte("SELECT 1;\n")}}},
		{"the stamp of another version", `"0001_init.sql" does not end with the statement that records its version`, fstest.MapFS{"0001_init.sql": stamped(2, "SELECT 1;")}},
		{"a statement after the stamp", `"0001_init.sql" does not end with the statement that records its version`, fstest.MapFS{"0001_init.sql": {Data: []byte(fmt.Sprintf(stampMigration, 1) + "\nSELECT 1;\n")}}},
		{"a second insert into the table", `"0001_init.sql" inserts into podium_schema_migrations 2 times`, fstest.MapFS{"0001_init.sql": stamped(1, "insert  into podium_schema_migrations (version) VALUES (7);")}},
	} {
		if files, err := migrationFiles(tc.fsys); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("migration files with %s: %+v, %v; want an error naming %q", tc.name, files, err, tc.want)
		}
	}
}
