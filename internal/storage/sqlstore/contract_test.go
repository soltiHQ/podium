package sqlstore

import (
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/storagecontract"
)

// TestContract runs the storage contract on the SQL store, as it runs on the
// in-memory store and on raft. The whole run shares one database, migrated once
// and emptied before each check, which the suite allows.
func TestContract(t *testing.T) {
	db := migrated(t)
	storagecontract.Run(t, func(t *testing.T) storage.Storage {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), "TRUNCATE "+strings.Join(entityTables, ", ")); err != nil {
			t.Fatalf("empty the tables: %v", err)
		}
		return openStore(t, Options{Primary: db})
	})
}
