package inmemory

import (
	"context"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

var (
	_ storage.ClockedSessionRotation = (*Store)(nil)
	_ storage.ClockedSessionRotation = (*state)(nil)
)

func (s *Store) RotateRefreshAt(ctx context.Context, sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.RotateRefreshAt(ctx, sessionID, newHash, newExpiresAt, updatedAt)
}

func (s *state) RotateRefreshAt(ctx context.Context, sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" || len(newHash) == 0 || newExpiresAt.IsZero() {
		return storage.ErrInvalidArgument
	}
	return s.sessions.Update(ctx, sessionID, func(cur *model.Session) (*model.Session, error) {
		if err := cur.RotateRefreshAt(newHash, newExpiresAt, updatedAt); err != nil {
			return nil, err
		}
		return cur, nil
	})
}
