package password

import (
	"context"
	"errors"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Request contains input credentials for password authentication.
type Request struct {
	Subject  string
	Password string
}

// Provider authenticates a principal using a password credential.
type Provider struct {
	store    storage.Storage
	resolver *rbac.Resolver
}

func New(store storage.Storage, resolver *rbac.Resolver) *Provider {
	return &Provider{store: store, resolver: resolver}
}

func (p *Provider) Kind() kind.Auth { return kind.Password }

// Authenticate validates subject+password and returns an identity with effective permissions.
func (p *Provider) Authenticate(ctx context.Context, req *Request) (*identity.Identity, error) {
	if p.store == nil || p.resolver == nil {
		return nil, storage.ErrInvalidArgument
	}
	if req == nil || req.Subject == "" || req.Password == "" {
		return nil, ErrInvalidCredentials
	}

	u, err := p.store.GetUserBySubject(ctx, req.Subject)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if u.Disabled() {
		return nil, ErrInvalidCredentials
	}

	cred, err := p.store.GetCredentialByUserAndAuth(ctx, u.ID(), kind.Password)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if err = credentials.VerifyPassword(cred, req.Password); err != nil {
		return nil, ErrInvalidCredentials
	}

	perms, err := p.resolver.ResolveUserPermissions(ctx, u)
	if err != nil {
		if errors.Is(err, rbac.ErrUnauthorized) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	return &identity.Identity{
		Subject:     u.Subject(),
		Email:       u.Email(),
		Name:        u.Name(),
		UserID:      u.ID(),
		Permissions: perms,
	}, nil
}
