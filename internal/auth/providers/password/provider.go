package password

import (
	"context"
	"errors"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Provider authenticates using (subject, password).
type Provider struct {
	store storage.Storage
}

// New creates a new password provider.
func New(store storage.Storage) *Provider {
	return &Provider{store: store}
}

// Kind returns the provider kind.
func (*Provider) Kind() enum.Auth { return enum.Password }

// Authenticate authenticates the user by subject/password.
//
// Contract:
//   - Does not leak which field was wrong (subject/password/user state).
//   - Uses Credential as (user <-> auth kind) binding.
//   - Uses Verifier as verification material store (bcrypt hash, params, etc).
//
// Errors:
//   - auth.ErrInvalidRequest for invalid wiring or wrong request type/kind.
//   - auth.ErrInvalidCredentials for subject/password mismatch (no field leakage).
//   - may return storage.ErrUnavailable / storage.ErrInternal from the backend.
func (p *Provider) Authenticate(ctx context.Context, req providers.Request) (*providers.Result, error) {
	if p == nil || p.store == nil {
		return nil, auth.ErrInvalidRequest
	}
	if req == nil || req.AuthKind() != enum.Password {
		return nil, auth.ErrInvalidRequest
	}

	r, ok := req.(Request)
	if !ok {
		return nil, auth.ErrInvalidRequest
	}
	if r.Subject == "" || r.Password == "" {
		return nil, auth.ErrInvalidCredentials
	}

	u, err := p.store.GetUserBySubject(ctx, r.Subject)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, auth.ErrInvalidCredentials
		}
		return nil, err
	}
	if u.Disabled() {
		return nil, auth.ErrInvalidCredentials
	}

	cred, err := p.store.GetCredentialByUserAndAuth(ctx, u.ID(), enum.Password)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, auth.ErrInvalidCredentials
		}
		return nil, err
	}

	ver, err := p.store.GetVerifierByCredential(ctx, cred.ID())
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, auth.ErrInvalidCredentials
		}
		return nil, err
	}
	if err = credentials.VerifyPassword(cred, ver, r.Password); err != nil {
		return nil, auth.ErrInvalidCredentials
	}

	return &providers.Result{
		User:       u,
		Credential: cred,
	}, nil
}
