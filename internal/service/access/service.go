package access

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/kind"
	iauth "github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/wire"
)

// Service implements shared authentication use-cases.
type Service struct {
	auth *wire.Auth
}

// New creates a new authentication service.
func New(authSvc *wire.Auth) *Service {
	if authSvc == nil {
		panic("access.Service: nil auth service")
	}
	return &Service{auth: authSvc}
}

// Login authenticates a user and returns issued tokens and identity.
func (s *Service) Login(ctx context.Context, req LoginRequest) (*identity.Identity, LoginResult, error) {
	if req.Subject == "" || req.Password == "" {
		return nil, LoginResult{}, iauth.ErrInvalidRequest
	}

	now := s.auth.Clock.Now()
	if s.auth.Limiter != nil && req.RateKey != "" {
		if err := s.auth.Limiter.Check(req.RateKey, now); err != nil {
			return nil, LoginResult{}, err
		}
	}

	pair, id, err := s.auth.Session.Login(ctx, kind.Password, req.Subject, req.Password)
	if err != nil {
		if s.auth.Limiter != nil && req.RateKey != "" {
			s.auth.Limiter.RecordFailure(req.RateKey, now)
		}
		return nil, LoginResult{}, err
	}
	if s.auth.Limiter != nil && req.RateKey != "" {
		s.auth.Limiter.Reset(req.RateKey)
	}
	return id, LoginResult{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		SessionID:    id.SessionID,
	}, nil
}

// GetPermissions returns all available permissions in the system.
func (s *Service) GetPermissions() []kind.Permission {
	return kind.All
}

// Logout revokes a session (idempotent / best-effort).
func (s *Service) Logout(ctx context.Context, req LogoutRequest) error {
	if req.SessionID == "" {
		return nil
	}
	if err := s.auth.Session.Revoke(ctx, req.SessionID); err != nil {
		return err
	}
	return nil
}
