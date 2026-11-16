package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// A credential's row holds its user and its auth kind beside the body, for the
// lookup: indexed together, not unique. The auth kind may be empty.

var (
	credentialKind   = kind[*model.Credential]{"podium_credentials", snapshot.UnmarshalCredentialJSON}
	upsertCredential = upsertSQL("podium_credentials", "created_at", "user_id", "auth", "body")
)

// UpsertCredential creates or replaces a credential.
func (w *writer) UpsertCredential(ctx context.Context, c *model.Credential) error {
	if err := storage.ValidateEntity(c); err != nil {
		return err
	}
	body, err := snapshot.MarshalCredentialJSON(c)
	if err != nil {
		return err
	}
	id, err := indexKey("credential ID", c.ID())
	if err != nil {
		return err
	}
	user, err := indexKey("credential's user ID", c.UserID())
	if err != nil {
		return err
	}
	auth := string(c.AuthKind())
	// The user and the auth kind share an index entry.
	if err := checkIndexEntry("credential's user ID and auth kind", c.UserID(), auth); err != nil {
		return err
	}
	created, err := columnTime("creation time", c.CreatedAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, upsertCredential, id, created, user, bin(auth), body)
	return err
}

// GetCredential returns the credential with the ID.
func (r *reader) GetCredential(ctx context.Context, id string) (*model.Credential, error) {
	return byID(ctx, r, credentialKind, id)
}

// GetCredentialByUserAndAuth returns the credential of the user of the auth
// kind: exactly, by bytes. Two with the same keys are a fault,
// storage.ErrInternal.
func (r *reader) GetCredentialByUserAndAuth(ctx context.Context, userID string, auth enum.Auth) (*model.Credential, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}
	return lookup(ctx, r, credentialKind, "user_id = ? AND auth = ?", bin(userID), bin(string(auth)))
}

// ListCredentialsByUser returns every credential of the user, in the listing
// order, never a page; nil for none. It reads the primary, as the lookups do.
func (r *reader) ListCredentialsByUser(ctx context.Context, userID string) ([]*model.Credential, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}
	w := &where{}
	w.add("user_id = ?", bin(userID))
	return queryAll(ctx, r, credentialKind, w, nil)
}

// DeleteCredential deletes the credential with the ID.
func (w *writer) DeleteCredential(ctx context.Context, id string) error {
	return w.deleteByID(ctx, credentialKind.table, id)
}

// UpsertCredential creates or replaces a credential, in a write transaction of
// its own.
func (s *Store) UpsertCredential(ctx context.Context, c *model.Credential) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertCredential(ctx, c) })
}

// DeleteCredential deletes the credential with the ID, in a write transaction of
// its own.
func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteCredential(ctx, id) })
}
