package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// A session's row holds its user beside the body, for the by-user listing and
// delete. A session is created once, then rotated and revoked: read-modify-writes
// of its body, through the model's methods, so that the rules are the in-memory
// store's. Nothing here names a session ID in an error: it is the session cookie.

var (
	sessionKind   = kind[*model.Session]{"podium_sessions", snapshot.UnmarshalSessionJSON}
	insertSession = insertIfAbsent("podium_sessions", "created_at", "user_id", "body")
)

// CreateSession creates a session; one with the ID there already is
// storage.ErrAlreadyExists, from the rows the statement inserted, so that the
// transaction stays usable.
func (w *writer) CreateSession(ctx context.Context, s *model.Session) error {
	if err := storage.ValidateEntity(s); err != nil {
		return err
	}
	body, err := snapshot.MarshalSessionJSON(s)
	if err != nil {
		return err
	}
	id, err := indexKey("session ID", s.ID())
	if err != nil {
		return err
	}
	user, err := indexKey("session's user ID", s.UserID())
	if err != nil {
		return err
	}
	created, err := columnTime("creation time", s.CreatedAt())
	if err != nil {
		return err
	}
	n, err := w.exec(ctx, insertSession, id, created, user, body)
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrAlreadyExists
	}
	return nil
}

// GetSession returns the session with the ID.
func (r *reader) GetSession(ctx context.Context, id string) (*model.Session, error) {
	return byID(ctx, r, sessionKind, id)
}

// ListSessionsByUser returns every session of the user, in the listing order,
// never a page; nil for none.
func (r *reader) ListSessionsByUser(ctx context.Context, userID string) ([]*model.Session, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}
	w := &where{}
	w.add("user_id = ?", bin(userID))
	return queryAll(ctx, r, sessionKind, w, nil)
}

// RotateRefresh replaces the refresh hash and the expiry of the session, and
// stamps the modification time with the time of the call.
func (w *writer) RotateRefresh(ctx context.Context, sessionID string, newHash []byte, newExpiresAt time.Time) error {
	return w.RotateRefreshAt(ctx, sessionID, newHash, newExpiresAt, time.Now())
}

// RotateRefreshAt is RotateRefresh at the caller's time: a zero updatedAt keeps
// the stored modification time, and the same hash and expiry change nothing.
func (w *writer) RotateRefreshAt(ctx context.Context, sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) error {
	if sessionID == "" || len(newHash) == 0 || newExpiresAt.IsZero() {
		return storage.ErrInvalidArgument
	}
	return w.modifySession(ctx, sessionID, func(cur *model.Session) error {
		return cur.RotateRefreshAt(newHash, newExpiresAt, updatedAt)
	})
}

// RevokeSession marks the session revoked; the first revocation wins.
func (w *writer) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time) error {
	if sessionID == "" || revokedAt.IsZero() {
		return storage.ErrInvalidArgument
	}
	return w.modifySession(ctx, sessionID, func(cur *model.Session) error { return cur.Revoke(revokedAt) })
}

// modifySession reads the session with the ID under a row lock, changes it
// through the model's method, and writes it back: storage.ErrNotFound for no
// row. A change the model refuses, or a result the codec cannot carry, is
// storage.ErrInvalidArgument, before any write.
func (w *writer) modifySession(ctx context.Context, id string, change func(*model.Session) error) error {
	var body []byte
	err := w.q.QueryRowContext(ctx, w.d.Rebind("SELECT body FROM "+sessionKind.table+" WHERE id = ? FOR UPDATE"), bin(id)).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ErrNotFound
	}
	if err != nil {
		return w.fail(ctx, err)
	}
	cur, err := sessionKind.decode(body)
	if err != nil {
		return err
	}
	if err := change(cur); err != nil {
		return fmt.Errorf("%w: sqlstore: the session: %v", storage.ErrInvalidArgument, err)
	}
	next, err := snapshot.MarshalSessionJSON(cur)
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, "UPDATE "+sessionKind.table+" SET body = ? WHERE id = ?", next, bin(id))
	return err
}

// DeleteSession deletes the session with the ID.
func (w *writer) DeleteSession(ctx context.Context, id string) error {
	return w.deleteByID(ctx, sessionKind.table, id)
}

// DeleteSessionsByUser deletes every session of the user; a user with none is no
// error.
func (w *writer) DeleteSessionsByUser(ctx context.Context, userID string) error {
	if userID == "" {
		return storage.ErrInvalidArgument
	}
	return w.deleteWhere(ctx, sessionKind.table, "user_id = ?", bin(userID))
}

// CreateSession creates a session, in a write transaction of its own.
func (s *Store) CreateSession(ctx context.Context, sess *model.Session) error {
	return s.write(ctx, func(w *writer) error { return w.CreateSession(ctx, sess) })
}

// RotateRefresh replaces the refresh hash and the expiry of the session, in a
// write transaction of its own.
func (s *Store) RotateRefresh(ctx context.Context, sessionID string, newHash []byte, newExpiresAt time.Time) error {
	return s.write(ctx, func(w *writer) error { return w.RotateRefresh(ctx, sessionID, newHash, newExpiresAt) })
}

// RotateRefreshAt is RotateRefresh at the caller's time, in a write transaction
// of its own.
func (s *Store) RotateRefreshAt(ctx context.Context, sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) error {
	return s.write(ctx, func(w *writer) error {
		return w.RotateRefreshAt(ctx, sessionID, newHash, newExpiresAt, updatedAt)
	})
}

// RevokeSession marks the session revoked, in a write transaction of its own.
func (s *Store) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time) error {
	return s.write(ctx, func(w *writer) error { return w.RevokeSession(ctx, sessionID, revokedAt) })
}

// DeleteSession deletes the session with the ID, in a write transaction of its
// own.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteSession(ctx, id) })
}

// DeleteSessionsByUser deletes every session of the user, in a write transaction
// of its own.
func (s *Store) DeleteSessionsByUser(ctx context.Context, userID string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteSessionsByUser(ctx, userID) })
}
