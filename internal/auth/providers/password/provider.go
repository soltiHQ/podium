package password

import (
	"context"
	"errors"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Provider authenticates.
type Provider struct {
	store storage.Storage
}

// New creates a new password provider.
func New(store storage.Storage) *Provider {
	return &Provider{store: store}
}

// Kind returns the provider kind.
func (*Provider) Kind() kind.Auth { return kind.Password }

// Authenticate authenticates the user.
func (p *Provider) Authenticate(ctx context.Context, req providers.Request) (*providers.Result, error) {
	if p.store == nil {
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

	cred, err := p.store.GetCredentialByUserAndAuth(ctx, u.ID(), kind.Password)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, auth.ErrInvalidCredentials
		}
		return nil, err
	}

	if err = credentials.VerifyPassword(cred, r.Password); err != nil {
		return nil, auth.ErrInvalidCredentials
	}
	return &providers.Result{User: u, Credential: cred}, nil
}
