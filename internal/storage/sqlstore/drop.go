package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	// presentTables lists, sorted, the names of the schema that exist as tables,
	// found through the search path as an unqualified statement finds them: a
	// view or a sequence of the name is no table of podium's.
	presentTables = func() string {
		values := make([]string, len(schemaTables))
		for i, name := range schemaTables {
			values[i] = "('" + name + "')"
		}
		return `SELECT t.name FROM (VALUES ` + strings.Join(values, ", ") + `) AS t (name)
  JOIN pg_class c ON c.oid = to_regclass(t.name)
 WHERE c.relkind IN ('r', 'p')
 ORDER BY t.name COLLATE "C"`
	}()

	// dropTables drops every table of the schema that exists, rows, indexes and
	// the identity's sequence with it.
	dropTables = `DROP TABLE IF EXISTS ` + strings.Join(schemaTables, ", ")
)

// lockMigrationsForDrop holds off, until the drop commits, every other session
// that touches podium_schema_migrations: a migrator, which waits for it, and the
// read of SchemaVersion with which a replica opens the store, which fails once
// the table is gone.
const lockMigrationsForDrop = `LOCK TABLE podium_schema_migrations IN ACCESS EXCLUSIVE MODE`

// setLockTimeout bounds, for the rest of the transaction, each lock a statement
// waits for.
const setLockTimeout = `SELECT set_config('lock_timeout', ?, true)`

// Dropped is what DropSchema dropped: the version of the schema, read under the
// lock, and its tables, sorted.
type Dropped struct {
	Version int
	Tables  []string
}

// dropLockTimeout bounds each lock DropSchema waits for: another session that
// holds one longer, a pg_dump or a transaction left open, makes it answer
// ErrLocked rather than wait without end.
var dropLockTimeout = 5 * time.Second

// DropSchema drops this podium's schema from db: every table its migrations
// create, and the rows in them, which nothing brings back but a backup. It drops
// nothing else: no other table, no view, no schema of the database's, not the
// database.
//
// It runs in one transaction at READ COMMITTED, each lock it waits for bounded
// by its lock timeout, past which it answers ErrLocked. It finds which of the
// schema's names exist as tables; with none it ends there, dropping nothing and
// asking the guard nothing. Tables without podium_schema_migrations are no schema
// podium migrated, which it refuses with ErrNotPodiumSchema. Otherwise it locks
// podium_schema_migrations ACCESS EXCLUSIVE, which waits for a migrator in flight
// and holds off the next one and the read with which a replica opens the store;
// reads the version under the lock and refuses a schema newer than this podium's
// with ErrSchemaNewer, as it can hold tables this podium does not know; asks
// guard, in the transaction, with the other tables unlocked, so that a deployment
// whose replicas run never queues behind the drop; locks every table of the
// schema ACCESS EXCLUSIVE and asks guard again, so that nothing a replica writes,
// a heartbeat or a take of the lease, lands between that answer and the drop;
// then drops the tables in one statement and commits. Guard's refusal comes back
// as it is; a nil guard asks nothing. It returns the version and the tables it
// dropped. The lock statements and the catalog lookup are Postgres's, as
// Migrate's are.
func DropSchema(ctx context.Context, db *sql.DB, d Dialect, guard func(context.Context, *sql.Tx) error) (Dropped, error) {
	files, err := migrationFiles(d.Migrations())
	if err != nil {
		return Dropped{}, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Dropped{}, classify(ctx, d, err)
	}
	// After the commit, the rollback is a no-op.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, d.Rebind(setLockTimeout), fmt.Sprintf("%dms", dropLockTimeout.Milliseconds())); err != nil {
		return Dropped{}, fmt.Errorf("sqlstore: set the lock timeout: %w", classify(ctx, d, err))
	}
	present, err := tablesPresent(ctx, tx, d)
	if err != nil || len(present) == 0 {
		return Dropped{}, err
	}
	if !slices.Contains(present, migrationsTable) {
		return Dropped{}, fmt.Errorf("%w: %s", ErrNotPodiumSchema, strings.Join(present, ", "))
	}
	if _, err := tx.ExecContext(ctx, lockMigrationsForDrop); err != nil {
		return Dropped{}, lockFailed(ctx, d, "podium_schema_migrations", err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, appliedVersion).Scan(&version); err != nil {
		return Dropped{}, fmt.Errorf("sqlstore: read the schema version: %w", classify(ctx, d, err))
	}
	if version > len(files) {
		return Dropped{}, checkVersion(version, len(files))
	}
	// A migrator that held the lock before may have added tables.
	if present, err = tablesPresent(ctx, tx, d); err != nil {
		return Dropped{}, err
	}
	if guard != nil {
		if err := guard(ctx, tx); err != nil {
			return Dropped{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `LOCK TABLE `+strings.Join(present, ", ")+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return Dropped{}, lockFailed(ctx, d, "the tables of the schema", err)
	}
	if guard != nil {
		if err := guard(ctx, tx); err != nil {
			return Dropped{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, dropTables); err != nil {
		return Dropped{}, lockFailed(ctx, d, "the tables of the schema, to drop them,", err)
	}
	if err := tx.Commit(); err != nil {
		return Dropped{}, fmt.Errorf("sqlstore: commit the drop of the schema: %w", classify(ctx, d, err))
	}
	return Dropped{Version: version, Tables: present}, nil
}

// lockFailed is the error of a statement of DropSchema that takes locks on
// what: ErrLocked when it waited past the lock timeout (SQLSTATE 55P03), its
// classified error otherwise.
func lockFailed(ctx context.Context, d Dialect, what string, err error) error {
	var pgErr *pgconn.PgError
	if ctx.Err() == nil && errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return fmt.Errorf("%w: %s, for longer than the lock timeout of %s", ErrLocked, what, dropLockTimeout)
	}
	return fmt.Errorf("sqlstore: lock %s: %w", what, classify(ctx, d, err))
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
