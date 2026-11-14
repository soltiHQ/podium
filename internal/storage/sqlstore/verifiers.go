package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// A verifier's row holds its credential's ID beside the body, for the lookup:
// indexed, not unique.

var (
	verifierKind   = kind[*model.Verifier]{"podium_verifiers", snapshot.UnmarshalVerifierJSON}
	upsertVerifier = upsertSQL("podium_verifiers", "created_at", "credential_id", "body")
)

// UpsertVerifier creates or replaces a verifier.
func (w *writer) UpsertVerifier(ctx context.Context, v *model.Verifier) error {
	if err := storage.ValidateEntity(v); err != nil {
		return err
	}
	body, err := snapshot.MarshalVerifierJSON(v)
	if err != nil {
		return err
	}
	id, err := indexKey("verifier ID", v.ID())
	if err != nil {
		return err
	}
	credential, err := indexKey("verifier's credential ID", v.CredentialID())
	if err != nil {
		return err
	}
	created, err := columnTime("creation time", v.CreatedAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, upsertVerifier, id, created, credential, body)
	return err
}

// GetVerifier returns the verifier with the ID.
func (r *reader) GetVerifier(ctx context.Context, id string) (*model.Verifier, error) {
	return byID(ctx, r, verifierKind, id)
}

// GetVerifierByCredential returns the verifier of the credential: exactly, by
// bytes. Two verifiers of one credential are a fault, storage.ErrInternal.
func (r *reader) GetVerifierByCredential(ctx context.Context, credentialID string) (*model.Verifier, error) {
	if credentialID == "" {
		return nil, storage.ErrInvalidArgument
	}
	return lookup(ctx, r, verifierKind, "credential_id = ?", bin(credentialID))
}

// DeleteVerifier deletes the verifier with the ID.
func (w *writer) DeleteVerifier(ctx context.Context, id string) error {
	return w.deleteByID(ctx, verifierKind.table, id)
}

// DeleteVerifierByCredential deletes every verifier of the credential, as memory
// does; a credential with none is no error.
func (w *writer) DeleteVerifierByCredential(ctx context.Context, credentialID string) error {
	if credentialID == "" {
		return storage.ErrInvalidArgument
	}
	return w.deleteWhere(ctx, verifierKind.table, "credential_id = ?", bin(credentialID))
}

// UpsertVerifier creates or replaces a verifier, in a write transaction of its
// own.
func (s *Store) UpsertVerifier(ctx context.Context, v *model.Verifier) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertVerifier(ctx, v) })
}

// DeleteVerifier deletes the verifier with the ID, in a write transaction of its
// own.
func (s *Store) DeleteVerifier(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteVerifier(ctx, id) })
}

// DeleteVerifierByCredential deletes every verifier of the credential, in a
// write transaction of its own.
func (s *Store) DeleteVerifierByCredential(ctx context.Context, credentialID string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteVerifierByCredential(ctx, credentialID) })
}
