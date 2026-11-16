package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// A user's row holds its subject beside the body, for the lookup: indexed, not
// unique, as in the in-memory store.

var (
	userKind   = kind[*model.User]{"podium_users", snapshot.UnmarshalUserJSON}
	upsertUser = upsertSQL("podium_users", "created_at", "subject", "body")
)

// UpsertUser creates or replaces a user.
func (w *writer) UpsertUser(ctx context.Context, u *model.User) error {
	if err := storage.ValidateEntity(u); err != nil {
		return err
	}
	body, err := snapshot.MarshalUserJSON(u)
	if err != nil {
		return err
	}
	id, err := indexKey("user ID", u.ID())
	if err != nil {
		return err
	}
	subject, err := indexKey("user's subject", u.Subject())
	if err != nil {
		return err
	}
	created, err := columnTime("creation time", u.CreatedAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, upsertUser, id, created, subject, body)
	return err
}

// GetUser returns the user with the ID.
func (r *reader) GetUser(ctx context.Context, id string) (*model.User, error) {
	return byID(ctx, r, userKind, id)
}

// GetUserBySubject returns the user with the subject: exactly, by bytes. Two
// users with one subject are a fault, storage.ErrInternal.
func (r *reader) GetUserBySubject(ctx context.Context, subject string) (*model.User, error) {
	if subject == "" {
		return nil, storage.ErrInvalidArgument
	}
	return lookup(ctx, r, userKind, "subject = ?", bin(subject))
}

// ListUsers returns a page of the users the filter matches.
func (r *reader) ListUsers(ctx context.Context, filter storage.UserFilter, opts storage.ListOptions) (*storage.UserListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, err := userQuery(filter)
	if err != nil {
		return nil, err
	}
	return listPage(ctx, r, q, opts)
}

// DeleteUser deletes the user with the ID.
func (w *writer) DeleteUser(ctx context.Context, id string) error {
	return w.deleteByID(ctx, userKind.table, id)
}

// UpsertUser creates or replaces a user, in a write transaction of its own.
func (s *Store) UpsertUser(ctx context.Context, u *model.User) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertUser(ctx, u) })
}

// ListUsers returns a page of the users the filter matches, from the replica for
// a read-only context.
func (s *Store) ListUsers(ctx context.Context, filter storage.UserFilter, opts storage.ListOptions) (*storage.UserListResult, error) {
	return routed(ctx, s, func(r *reader) (*storage.UserListResult, error) { return r.ListUsers(ctx, filter, opts) })
}

// DeleteUser deletes the user with the ID, in a write transaction of its own.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteUser(ctx, id) })
}
