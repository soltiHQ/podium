package overlay

import (
	"context"
	"errors"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// CreateSession refuses a session the transaction already sees with
// ErrAlreadyExists, as the stores do.
func (t *Tx) CreateSession(ctx context.Context, s *model.Session) error {
	if s == nil {
		return storage.ErrInvalidArgument
	}
	msg := wire.SessionToProto(s)
	row, err := wire.SessionFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	switch _, err := t.GetSession(ctx, row.ID()); {
	case err == nil:
		return storage.ErrAlreadyExists
	case !errors.Is(err, storage.ErrNotFound):
		return err
	}
	t.sessions.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_SessionCreate{SessionCreate: msg}})
	return nil
}

func (t *Tx) GetSession(ctx context.Context, id string) (*model.Session, error) {
	return t.sessions.get(ctx, id, t.base.GetSession)
}

func (t *Tx) ListSessionsByUser(ctx context.Context, userID string) ([]*model.Session, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}
	rows, err := t.base.ListSessionsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return merged(rows, &t.sessions, func(s *model.Session) bool { return s.UserID() == userID }), nil
}

func (t *Tx) RotateRefresh(ctx context.Context, sessionID string, newHash []byte, newExpiresAt time.Time) error {
	return t.RotateRefreshAt(ctx, sessionID, newHash, newExpiresAt, time.Now())
}

// RotateRefreshAt rotates the session with the values its operation carries, as the
// state machine does, so the transaction reads what a commit stores.
func (t *Tx) RotateRefreshAt(ctx context.Context, sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" || len(newHash) == 0 || newExpiresAt.IsZero() {
		return storage.ErrInvalidArgument
	}
	cur, err := t.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	op := RotateRefreshOp(sessionID, newHash, newExpiresAt, updatedAt)
	hash, expiresAt, at := rotation(op.GetSessionRotateRefresh())
	if err := cur.RotateRefreshAt(hash, expiresAt, at); err != nil {
		return err
	}
	if err := valid(cur); err != nil {
		return err
	}
	t.sessions.put(cur)
	t.record(op)
	return nil
}

func (t *Tx) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" || revokedAt.IsZero() {
		return storage.ErrInvalidArgument
	}
	cur, err := t.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	msg := &raftv1.SessionRevokeMsg{Id: sessionID, RevokedAtNs: revokedAt.UnixNano()}
	if err := cur.Revoke(revocation(msg)); err != nil {
		return err
	}
	if err := valid(cur); err != nil {
		return err
	}
	t.sessions.put(cur)
	t.record(&raftv1.Op{Op: &raftv1.Op_SessionRevoke{SessionRevoke: msg}})
	return nil
}

func (t *Tx) DeleteSession(ctx context.Context, id string) error {
	if err := t.sessions.remove(ctx, id, t.base.GetSession); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_SessionDelete{SessionDelete: id}})
	return nil
}

func (t *Tx) DeleteSessionsByUser(ctx context.Context, userID string) error {
	if userID == "" {
		return storage.ErrInvalidArgument
	}
	inBase, err := t.base.ListSessionsByUser(ctx, userID)
	if err != nil {
		return err
	}
	if t.sessions.removeAll(inBase, func(s *model.Session) bool { return s.UserID() == userID }) {
		t.record(&raftv1.Op{Op: &raftv1.Op_SessionDeleteByUser{SessionDeleteByUser: userID}})
	}
	return nil
}
