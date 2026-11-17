package sqlstore

import (
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/sqlstore/pgtest"
	"github.com/soltiHQ/control-plane/internal/storage/storagecontract"
)

// TestContract runs the storage contract on the SQL store, as it runs on the
// in-memory store and on raft. The whole run shares one database, migrated once
// and emptied before each check, which the suite allows.
//
// The store runs on a role that may only read and write rows, pgtest.DMLRole,
// as a deployment whose migrations a tool of its own applies runs podium, and
// the database's owner migrates and empties it: a statement of the store that
// needs more fails here.
func TestContract(t *testing.T) {
	databaseURL := pgtest.NewDatabase(t)
	// Before the migration: the tables get the role's grants as they are made.
	dmlURL := pgtest.DMLRole(t, databaseURL)
	owner := pgtest.Open(t, databaseURL)
	if err := Migrate(t.Context(), owner, Postgres{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db := pgtest.Open(t, dmlURL)
	storagecontract.Run(t, func(t *testing.T) storage.Storage {
		t.Helper()
		if _, err := owner.ExecContext(t.Context(), "TRUNCATE "+strings.Join(entityTables, ", ")); err != nil {
			t.Fatalf("empty the tables: %v", err)
		}
		return openStore(t, Options{Primary: db})
	})
}
