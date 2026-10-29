package durable

import (
	"context"
	"fmt"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

var _ storage.ClockedSessionRotation = (*Store)(nil)

func (d *Store) RotateRefreshAt(ctx context.Context, sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) error {
	return d.WithTx(ctx, func(tx storage.Storage) error {
		clocked, ok := tx.(storage.ClockedSessionRotation)
		if !ok {
			return fmt.Errorf("%w: explicit session rotation timestamps are unsupported", storage.ErrInternal)
		}
		return clocked.RotateRefreshAt(ctx, sessionID, newHash, newExpiresAt, updatedAt)
	})
}
