package sqlstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// listing runs read on the database that serves a listing, a scan or a count
// of the store outside a transaction: the replica, when the store has one and
// ctx is marked storage.MarkReadOnly, and the primary otherwise. A marked read
// that fails on the replica with storage.ErrUnavailable while ctx is live runs
// once more, on the primary: a replica that is down, unreachable or cancelling
// reads does not fail a listing the primary can answer. Any other error of the
// replica, and the error of a done ctx, comes back as it is. A point lookup
// reads s.primary, and a transaction its own handle, never the replica:
// runners, auth and transactions decide on what they read, and a replica may
// lag.
func (s *Store) listing(ctx context.Context, read func(db *sql.DB) error) error {
	if s.replica == nil || !storage.IsReadOnly(ctx) {
		return read(s.primary)
	}
	err := read(s.replica)
	if errors.Is(err, storage.ErrUnavailable) && ctx.Err() == nil {
		return read(s.primary)
	}
	return err
}
