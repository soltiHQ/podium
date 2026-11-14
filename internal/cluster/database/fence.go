package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// fenceRead reads the lease row in a write transaction, FOR SHARE: a takeover,
// and a renewal, wait until the transaction ends, and a fence that runs after a
// takeover reads the row it left.
const fenceRead = `SELECT epoch, holder FROM podium_leader WHERE id = 1 FOR SHARE`

// The fence's two refusals. Both are cluster.ErrNotLeader: the fence runs
// before the transaction's first write, so nothing was written, as raft refuses
// a write whose term moved before it proposed it.
var (
	// errNotHolder refuses a write of a replica that does not hold the lease,
	// without a statement.
	errNotHolder = fmt.Errorf("database: this replica does not hold the lease: %w", cluster.ErrNotLeader)
	// errLeaseMoved refuses a write of a replica whose lease another replica
	// took, which it had not seen yet.
	errLeaseMoved = fmt.Errorf("database: the lease moved to another replica: %w", cluster.ErrNotLeader)
)

// Check implements sqlstore.Fence. It runs at the start of each write
// transaction of the store, before its first write, and refuses the
// transaction of a replica that does not hold the lease with errNotHolder. The
// holder reads the lease row FOR SHARE and compares its epoch and holder with
// its own term's: a row that moved, a missing one included, is refused with
// errLeaseMoved, and steps this replica down at once. The share lock holds the
// row until the transaction ends, so that no takeover lands while the write
// runs. A statement that fails answers as failed classifies it.
func (l *Leadership) Check(ctx context.Context, tx *sql.Tx) error {
	t := l.current()
	if t == nil {
		return errNotHolder
	}
	var (
		epoch  int64
		holder string
	)
	err := tx.QueryRowContext(ctx, fenceRead).Scan(&epoch, &holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The row is gone: this replica holds nothing.
	case err != nil:
		return failed(ctx, err)
	}
	if epoch != t.epoch || holder != l.id {
		if l.stepDown(t) {
			l.logger.Warn().Int64("epoch", t.epoch).Int64("lease_epoch", epoch).Str("holder_id", holder).
				Msg("database: a write found the lease moved to another replica: stepping down")
		}
		return errLeaseMoved
	}
	// A step-down while the row was read leaves the write to the replica that
	// takes over; the share lock then holds that takeover until the rollback.
	if l.current() != t {
		return errNotHolder
	}
	return nil
}
