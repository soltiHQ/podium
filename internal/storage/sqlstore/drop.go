package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// schemaTables are the tables of this podium's schema, sorted: every table its
// migrations create, which DropSchema drops. A migration that adds a table adds
// it here, as TestMigrateCreatesSchema checks.
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

// migrationsTable is the table of the migrations applied, which DropSchema
// locks.
const migrationsTable = "podium_schema_migrations"

// The statements of DropSchema, built from schemaTables, podium's own names,
// never input.
var (
	// presentTables lists, sorted, the tables of the schema that exist, found
	// through the search path as an unqualified statement finds them.
	presentTables = func() string {
		values := make([]string, len(schemaTables))
		for i, name := range schemaTables {
			values[i] = "('" + name + "')"
		}
		return `SELECT name FROM (VALUES ` + strings.Join(values, ", ") + `) AS t (name)
 WHERE to_regclass(name) IS NOT NULL
 ORDER BY name COLLATE "C"`
	}()

	// dropTables drops every table of the schema that exists, rows, indexes and
	// the identity's sequence with it.
	dropTables = `DROP TABLE IF EXISTS ` + strings.Join(schemaTables, ", ")
)

// lockMigrationsForDrop holds off, until the drop commits, every other session
// that touches podium_schema_migrations: a migrator, which waits for it, and the
// read of SchemaVersion with which a replica opens the store, which then finds
// no schema.
const lockMigrationsForDrop = `LOCK TABLE podium_schema_migrations IN ACCESS EXCLUSIVE MODE`

// DropSchema drops this podium's schema from db: every table its migrations
// create, and the rows in them, which nothing brings back but a backup. It drops
// nothing else: no other table, no schema of the database's, not the database.
//
// It runs in one transaction at READ COMMITTED. It finds which of the tables
// exist; with none it ends there, answering no table and no error, and asks the
// guard nothing. Otherwise it locks podium_schema_migrations ACCESS EXCLUSIVE,
// when that table exists, which waits for a migrator in flight; reads the version
// under the lock and refuses a schema newer than this podium's with
// ErrSchemaNewer, as it can hold tables this podium does not know; then asks
// guard, in the transaction and under the lock, before any table goes, and
// answers guard's refusal as it is (a nil guard asks nothing); then drops the
// tables in one statement and commits. It returns the tables it dropped, sorted.
// The lock statement and the catalog lookup are Postgres's, as Migrate's are.
func DropSchema(ctx context.Context, db *sql.DB, d Dialect, guard func(context.Context, *sql.Tx) error) ([]string, error) {
	files, err := migrationFiles(d.Migrations())
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, classify(ctx, d, err)
	}
	// After the commit, the rollback is a no-op.
	defer func() { _ = tx.Rollback() }()

	present, err := tablesPresent(ctx, tx, d)
	if err != nil || len(present) == 0 {
		return nil, err
	}
	if slices.Contains(present, migrationsTable) {
		if _, err := tx.ExecContext(ctx, lockMigrationsForDrop); err != nil {
			return nil, fmt.Errorf("sqlstore: lock podium_schema_migrations: %w", classify(ctx, d, err))
		}
		var version int
		if err := tx.QueryRowContext(ctx, appliedVersion).Scan(&version); err != nil {
			return nil, fmt.Errorf("sqlstore: read the schema version: %w", classify(ctx, d, err))
		}
		if version > len(files) {
			return nil, checkVersion(version, len(files))
		}
		// A migrator that held the lock before may have added tables.
		if present, err = tablesPresent(ctx, tx, d); err != nil {
			return nil, err
		}
	}
	if guard != nil {
		if err := guard(ctx, tx); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, dropTables); err != nil {
		return nil, fmt.Errorf("sqlstore: drop the schema: %w", classify(ctx, d, err))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlstore: commit the drop of the schema: %w", classify(ctx, d, err))
	}
	return present, nil
}

// tablesPresent returns, sorted, the tables of the schema that exist, nil for
// none.
func tablesPresent(ctx context.Context, tx *sql.Tx, d Dialect) ([]string, error) {
	rows, err := tx.QueryContext(ctx, presentTables)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: list the tables of the schema: %w", classify(ctx, d, err))
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlstore: list the tables of the schema: %w", classify(ctx, d, err))
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlstore: list the tables of the schema: %w", classify(ctx, d, err))
	}
	return names, nil
}
