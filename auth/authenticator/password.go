package authenticator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/soltiHQ/control-plane/auth"
	"github.com/soltiHQ/control-plane/auth/credentials"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// PasswordAuthenticator authenticates a principal using a password credential and issues a signed access token.
type PasswordAuthenticator struct {
	store  storage.Storage
	issuer auth.Issuer
	cfg    auth.JWTConfig
}

// NewPasswordAuthenticator creates a password-based authenticator.
func NewPasswordAuthenticator(store storage.Storage, issuer auth.Issuer, cfg auth.JWTConfig) *PasswordAuthenticator {
	return &PasswordAuthenticator{
		store:  store,
		issuer: issuer,
		cfg:    cfg,
	}
}

// Authenticate validates a password credential and issues a JWT token containing permissions.
func (a *PasswordAuthenticator) Authenticate(ctx context.Context, req *Request) (string, *auth.Identity, error) {
	if a.store == nil || a.issuer == nil {
		return "", nil, storage.ErrInvalidArgument
	}
	if req == nil || req.Subject == "" || req.Password == "" {
		return "", nil, ErrInvalidCredentials
	}

	u, err := a.store.GetUserBySubject(ctx, req.Subject)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, err
	}
	if u.Disabled() {
		return "", nil, ErrInvalidCredentials
	}

	cred, err := a.store.GetCredentialByUserAndType(ctx, u.ID(), domain.CredentialTypePassword)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, err
	}
	if err = credentials.VerifyPassword(cred, req.Password); err != nil {
		if errors.Is(err, credentials.ErrPasswordMismatch) {
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, ErrInvalidCredentials
	}

	roleIDs := u.RoleIDsAll()
	var roles []*domain.RoleModel
	if len(roleIDs) != 0 {
		roles, err = a.store.GetRoles(ctx, roleIDs)
		if err != nil {
			return "", nil, err
		}
	}
	perms := collectPermissions(roles)
	if len(perms) == 0 {
		return "", nil, ErrUnauthorized
	}

	now := time.Now()
	id := &auth.Identity{
		Issuer:      a.cfg.Issuer,
		Audience:    []string{a.cfg.Audience},
		Subject:     u.Subject(),
		UserID:      u.ID(),
		Name:        u.Name(),
		Email:       u.Email(),
		IssuedAt:    now,
		NotBefore:   now,
		ExpiresAt:   now.Add(a.cfg.TokenTTL),
		TokenID:     newTokenID(),
		Permissions: perms,
	}

	token, err := a.issuer.Issue(ctx, id)
	if err != nil {
		return "", nil, err
	}
	return token, id, nil
}

func newTokenID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
