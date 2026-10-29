package session

import (
	"context"
	"errors"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// AuthorizeAccess checks the durable session after cryptographic verification.
// Revocation, account disablement and permission changes take effect on the
// next request rather than waiting for the access token's expiry.
func (s *Service) AuthorizeAccess(ctx context.Context, id *identity.Identity) (*identity.Identity, error) {
	if err := s.ensureReady(); err != nil {
		return nil, err
	}
	if id == nil || id.SessionID == "" || id.UserID == "" {
		return nil, auth.ErrInvalidToken
	}
	sess, err := s.store.GetSession(ctx, id.SessionID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, auth.ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if sess.Revoked() || sess.Expired(s.clock.Now()) || sess.UserID() != id.UserID {
		return nil, auth.ErrRevoked
	}
	u, err := s.store.GetUser(ctx, id.UserID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, auth.ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if u.Disabled() {
		return nil, auth.ErrRevoked
	}
	perms, err := s.rbac.ResolveUserPermissions(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(perms) == 0 {
		return nil, auth.ErrUnauthorized
	}
	current := *id
	current.Name = u.Name()
	current.Email = u.Email()
	current.Subject = u.Subject()
	current.Permissions = perms
	return &current, nil
}
