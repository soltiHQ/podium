package overlay

import (
	"context"
	"errors"
	"fmt"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func (t *Tx) UpsertCredential(_ context.Context, c *model.Credential) error {
	if c == nil {
		return storage.ErrInvalidArgument
	}
	msg := wire.CredentialToProto(c)
	row, err := wire.CredentialFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.credentials.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_CredentialUpsert{CredentialUpsert: msg}})
	return nil
}

func (t *Tx) GetCredential(ctx context.Context, id string) (*model.Credential, error) {
	return t.credentials.get(ctx, id, t.base.GetCredential)
}

func (t *Tx) GetCredentialByUserAndAuth(ctx context.Context, userID string, auth enum.Auth) (*model.Credential, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}
	return unique(&t.credentials,
		func(c *model.Credential) bool { return c.UserID() == userID && c.AuthKind() == auth },
		func() (*model.Credential, error) { return t.base.GetCredentialByUserAndAuth(ctx, userID, auth) },
		fmt.Sprintf("credential for user %q auth %q", userID, auth))
}

func (t *Tx) ListCredentialsByUser(ctx context.Context, userID string) ([]*model.Credential, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}
	rows, err := t.base.ListCredentialsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return merged(rows, &t.credentials, func(c *model.Credential) bool { return c.UserID() == userID }), nil
}

func (t *Tx) DeleteCredential(ctx context.Context, id string) error {
	if err := t.credentials.remove(ctx, id, t.base.GetCredential); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_CredentialDelete{CredentialDelete: id}})
	return nil
}

func (t *Tx) UpsertVerifier(_ context.Context, v *model.Verifier) error {
	if v == nil {
		return storage.ErrInvalidArgument
	}
	msg := wire.VerifierToProto(v)
	row, err := wire.VerifierFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.verifiers.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_VerifierUpsert{VerifierUpsert: msg}})
	return nil
}

func (t *Tx) GetVerifier(ctx context.Context, id string) (*model.Verifier, error) {
	return t.verifiers.get(ctx, id, t.base.GetVerifier)
}

func (t *Tx) GetVerifierByCredential(ctx context.Context, credentialID string) (*model.Verifier, error) {
	if credentialID == "" {
		return nil, storage.ErrInvalidArgument
	}
	return unique(&t.verifiers,
		func(v *model.Verifier) bool { return v.CredentialID() == credentialID },
		func() (*model.Verifier, error) { return t.base.GetVerifierByCredential(ctx, credentialID) },
		fmt.Sprintf("verifier for credential %q", credentialID))
}

func (t *Tx) DeleteVerifier(ctx context.Context, id string) error {
	if err := t.verifiers.remove(ctx, id, t.base.GetVerifier); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_VerifierDelete{VerifierDelete: id}})
	return nil
}

// DeleteVerifierByCredential finds the base's verifier of the credential with the
// base's lookup, so a base that holds two fails it, as that lookup does.
func (t *Tx) DeleteVerifierByCredential(ctx context.Context, credentialID string) error {
	if credentialID == "" {
		return storage.ErrInvalidArgument
	}
	var inBase []*model.Verifier
	switch v, err := t.base.GetVerifierByCredential(ctx, credentialID); {
	case err == nil:
		inBase = append(inBase, v)
	case !errors.Is(err, storage.ErrNotFound):
		return err
	}
	if t.verifiers.removeAll(inBase, func(v *model.Verifier) bool { return v.CredentialID() == credentialID }) {
		t.record(&raftv1.Op{Op: &raftv1.Op_VerifierDeleteByCred{VerifierDeleteByCred: credentialID}})
	}
	return nil
}
