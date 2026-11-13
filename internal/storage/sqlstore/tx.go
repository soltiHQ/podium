package sqlstore

import (
	"context"
	"database/sql"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// handle is a write transaction as its callback sees it: every read and write
// of the callback runs on tx, on the primary, and a transaction the callback
// opens is this one.
type handle struct {
	store *Store
	tx    *sql.Tx
}

// transact runs fn in a write transaction of its own: WithTx's, or the one a
// write method called outside WithTx runs in. It waits for the store's gate,
// honouring ctx; begins the transaction on the primary at READ COMMITTED; has
// the fence check it, returning a refusal unchanged with fn never run; runs fn
// once; and commits unless fn failed or ctx is done by then, which roll the
// transaction back. fn's error comes back unchanged.
//
// fn must use the handle, never the store: a write of the store would wait for
// the gate fn holds until ctx is done. A caller that wraps its own callback in
// fn, as WithTx does, checks that callback for nil first.
func (s *Store) transact(ctx context.Context, fn func(h *handle) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.gate }()

	tx, err := s.primary.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return s.fail(ctx, err)
	}
	ended := false
	defer func() {
		if !ended {
			_ = tx.Rollback()
		}
	}()
	if err := s.fence.Check(ctx, tx); err != nil {
		return err
	}
	if err := fn(&handle{store: s, tx: tx}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ended = true
	if err := tx.Commit(); err != nil {
		return s.fail(ctx, err)
	}
	return nil
}

// transact runs fn in the transaction h is: a transaction inside a transaction
// is the outer one, without a savepoint, so fn's error rolls nothing back by
// itself, and the fence, which checked the outer one, does not check it again.
func (h *handle) transact(ctx context.Context, fn func(h *handle) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(h)
}
