package sqlstore

import (
	"context"
	"database/sql"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// listing runs read on the database that serves a listing, a scan or a count
// of the store outside a transaction: the replica, when the store has one and
// ctx is marked storage.MarkReadOnly, and the primary otherwise. A point lookup
// reads s.primary, and a transaction its own handle, never the replica: runners,
// auth and transactions decide on what they read, and a replica may lag.
func (s *Store) listing(ctx context.Context, read func(db *sql.DB) error) error {
	if s.replica != nil && storage.IsReadOnly(ctx) {
		return read(s.replica)
	}
	return read(s.primary)
}
