package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
)

// The migrator is podium's own. It applies a dialect's migrations in version
// order, each file in a transaction of its own, under a lock on
// podium_schema_migrations, which records the version of each file applied. So
// replicas that start together migrate once: each waits for the lock, then
// reads the version the others reached. The lock statement and the catalog
// lookup of SchemaVersion are Postgres's.
//
// Each file records its own version, as its last statement (stampMigration),
// and the migrator records nothing itself: so a tool of the deployment's own
// that applies the files instead, as Bytebase, leaves the schema at the version
// podium reads, as the migrator does. A file whose last statement records its
// version holds the whole migration once it is recorded, whoever ran it, and a
// tool that stops at the first failing statement records nothing.

const (
	// createMigrationsTable creates podium_schema_migrations. The migrator
	// creates it before the first migration, as it must lock the table, and
	// the first migration repeats the statement, so that the file holds the
	// whole schema.
	createMigrationsTable = `CREATE TABLE IF NOT EXISTS podium_schema_migrations (
    version    integer     PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);`

	// lockMigrations makes the migrators of a database take turns: EXCLUSIVE
	// conflicts with itself and with every write, and lets a plain read, as
	// SchemaVersion's, through.
	lockMigrations = `LOCK TABLE podium_schema_migrations IN EXCLUSIVE MODE`

	// migrationsTableExists finds podium_schema_migrations as an unqualified
	// statement would, through the search path.
	migrationsTableExists = `SELECT to_regclass('podium_schema_migrations') IS NOT NULL`

	appliedVersion = `SELECT coalesce(max(version), 0) FROM podium_schema_migrations`

	// stampMigration is the last statement of migration file N, N for its %d,
	// on a line of its own: the file records its own version. ON CONFLICT
	// leaves a version the table holds already as it is.
	stampMigration = `INSERT INTO podium_schema_migrations (version) VALUES (%d) ON CONFLICT (version) DO NOTHING;`
)

var (
	// migrationName is the name of a migration file, its version first.
	migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

	// insertsVersion finds the statements that insert into
	// podium_schema_migrations: a file has one, stampMigration.
	insertsVersion = regexp.MustCompile(`(?i)\binsert\s+into\s+podium_schema_migrations\b`)
)

// migration is one file of a dialect's migrations.
type migration struct {
	version int
	name    string
	sql     string
}

// migrationFiles reads the migrations at the root of fsys, in version order:
// each named NNNN_name.sql, versions 1, 2, 3… without a gap or a repeat, and
// stamped (checkStamp).
func migrationFiles(fsys fs.FS) ([]migration, error) {
	if fsys == nil {
		return nil, errors.New("sqlstore: the dialect has no migrations")
	}
	// ReadDir sorts the entries by name, and so the zero-padded versions.
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("sqlstore: read the migrations: %w", err)
	}
	var files []migration
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			return nil, fmt.Errorf("sqlstore: migration %q is not a file named NNNN_name.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		if want := len(files) + 1; version != want {
			return nil, fmt.Errorf("sqlstore: migration %q has version %d, want %d: versions run 1, 2, 3… without a gap or a repeat", e.Name(), version, want)
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("sqlstore: read migration %q: %w", e.Name(), err)
		}
		file := migration{version: version, name: e.Name(), sql: string(body)}
		if err := checkStamp(file); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, errors.New("sqlstore: the dialect has no migrations")
	}
	return files, nil
}

// checkStamp checks that m records its own version: that its last line other
// than a blank line or a -- comment is stampMigration of its version, and that
// no other statement of it inserts into podium_schema_migrations.
func checkStamp(m migration) error {
	var code []string // m's lines, each without its -- comment
	for line := range strings.Lines(m.sql) {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			code = append(code, line)
		}
	}
	want := fmt.Sprintf(stampMigration, m.version)
	if len(code) == 0 || code[len(code)-1] != want {
		return fmt.Errorf("sqlstore: migration %q does not end with the statement that records its version, on a line of its own: %s", m.name, want)
	}
	if n := len(insertsVersion.FindAllStringIndex(strings.Join(code, "\n"), -1)); n != 1 {
		return fmt.Errorf("sqlstore: migration %q inserts into podium_schema_migrations %d times: once, with its last statement, which records its version", m.name, n)
	}
	return nil
}

// LatestVersion returns the version of d's newest migration: the version of
// the schema this podium runs on. It checks d's migrations as Migrate does.
func LatestVersion(d Dialect) (int, error) {
	files, err := migrationFiles(d.Migrations())
	if err != nil {
		return 0, err
	}
	return len(files), nil
}

// SchemaVersion returns the version of db's schema: the latest migration
// podium_schema_migrations records, 0 for a database without the table, as an
// empty one. It reads and writes nothing else. Its catalog lookup and its error
// classification are those of the Postgres dialect.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, migrationsTableExists).Scan(&exists); err != nil {
		return 0, classify(ctx, Postgres{}, err)
	}
	if !exists {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(ctx, appliedVersion).Scan(&version); err != nil {
		return 0, classify(ctx, Postgres{}, err)
	}
	return version, nil
}

// Migrate brings db's schema to the latest of d's migrations. A database that
// is there already gets no statement but SchemaVersion's reads; one whose schema
// is newer gets none either, and Migrate answers ErrSchemaNewer. Otherwise it
// applies each missing migration in a transaction of its own, under the lock on
// podium_schema_migrations, where the migration records its version: a
// migration that fails rolls back alone, the ones before it kept, and so does
// one that leaves the schema at another version than its own. Migrators that
// run at once take turns.
func Migrate(ctx context.Context, db *sql.DB, d Dialect) error {
	files, err := migrationFiles(d.Migrations())
	if err != nil {
		return err
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if version >= len(files) {
		return checkVersion(version, len(files))
	}
	if err := createMigrations(ctx, db, d); err != nil {
		return err
	}
	for {
		done, err := migrateNext(ctx, db, d, files)
		if err != nil || done {
			return err
		}
	}
}

// createMigrations creates podium_schema_migrations, to lock. Two sessions that
// create one table at once can both find it missing, and the one that loses
// then fails, in Postgres, on a unique index of the catalog (pg_type) or with
// "already exists": the table exists then, made by the other, and the failure is
// no failure.
func createMigrations(ctx context.Context, db *sql.DB, d Dialect) error {
	_, err := db.ExecContext(ctx, createMigrationsTable)
	if err == nil {
		return nil
	}
	var exists bool
	if db.QueryRowContext(ctx, migrationsTableExists).Scan(&exists) == nil && exists {
		return nil
	}
	return fmt.Errorf("sqlstore: create podium_schema_migrations: %w", classify(ctx, d, err))
}

// migrateNext applies, under the lock, the migration after the version
// podium_schema_migrations records, in a transaction of its own, and reports
// whether there was none left to apply.
func migrateNext(ctx context.Context, db *sql.DB, d Dialect, files []migration) (done bool, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, classify(ctx, d, err)
	}
	// After the commit, the rollback is a no-op.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, lockMigrations); err != nil {
		return false, fmt.Errorf("sqlstore: lock podium_schema_migrations: %w", classify(ctx, d, err))
	}
	// At READ COMMITTED each statement reads what committed before it began,
	// so this one sees what the migrator that held the lock before recorded.
	var version int
	if err := tx.QueryRowContext(ctx, appliedVersion).Scan(&version); err != nil {
		return false, fmt.Errorf("sqlstore: read the schema version: %w", classify(ctx, d, err))
	}
	if version >= len(files) {
		return true, checkVersion(version, len(files))
	}
	next := files[version]
	if _, err := tx.ExecContext(ctx, next.sql); err != nil {
		return false, fmt.Errorf("sqlstore: migration %s: %w", next.name, classify(ctx, d, err))
	}
	// The file recorded its version, as its last statement: the schema is at
	// it now, or the file did something else than it says, and rolls back.
	var reached int
	if err := tx.QueryRowContext(ctx, appliedVersion).Scan(&reached); err != nil {
		return false, fmt.Errorf("sqlstore: read the schema version after migration %s: %w", next.name, classify(ctx, d, err))
	}
	if reached != next.version {
		return false, fmt.Errorf("sqlstore: migration %s left the schema at version %d, want %d", next.name, reached, next.version)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("sqlstore: commit migration %s: %w", next.name, classify(ctx, d, err))
	}
	return false, nil
}

// checkVersion refuses a schema version other than latest, this podium's:
// ErrSchemaNewer for a newer one, ErrSchemaOlder for an older one, naming both.
func checkVersion(version, latest int) error {
	switch {
	case version > latest:
		return fmt.Errorf("%w: the database is at version %d, this podium at version %d", ErrSchemaNewer, version, latest)
	case version < latest:
		return fmt.Errorf("%w: the database is at version %d, this podium at version %d", ErrSchemaOlder, version, latest)
	}
	return nil
}
