package access

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	iauth "github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Service implements shared authentication use-cases.
type Service struct {
	auth *kit.Auth

	logger zerolog.Logger
	store  storage.Storage
}

// New creates a new authentication service.
func New(authSvc *kit.Auth, store storage.Storage, logger zerolog.Logger) *Service {
	if authSvc == nil {
		panic("access.Service: nil auth service")
	}
	if store == nil {
		panic("access.Service: nil storage")
	}
	if authSvc.Limiter == nil {
		panic("access.Service: nil login limiter")
	}
	if authSvc.Devices == nil {
		panic("access.Service: nil device cookie signer")
	}
	return &Service{
		logger: logger.With().Str("service", "access").Logger(),
		auth:   authSvc,
		store:  store,
	}
}

// Login authenticates a user and returns issued tokens and identity.
//
// The subject is normalized once, the way user storage normalizes it, for the
// limiter, the device cookie and the password check alike. An attempt with a
// device cookie valid for the subject comes from a known device and counts on
// its device key alone; any other attempt counts on its account key and on its
// client IP key. Before the password check, the attempt reserves a slot on its
// keys, so attempts in flight count like failures: an attempt is refused with
// ErrRateLimited while, for any of its keys, failures plus attempts in flight
// reach the limit. Only a wrong subject or password (ErrInvalidCredentials)
// records a failure on the keys; the failure that blocks one of them comes
// back joined with ErrRateLimitReached. Any other error, such as a storage
// outage, releases the slots. A successful login resets the device key of a
// known device, or else the account key, never the client IP, and returns a
// new device cookie.
func (s *Service) Login(ctx context.Context, req LoginRequest) (*identity.Identity, LoginResult, error) {
	subject := model.NormalizeSubject(req.Subject)
	if subject == "" || req.Password == "" {
		return nil, LoginResult{}, iauth.ErrInvalidRequest
	}

	now := s.auth.Clock.Now()
	reset := ratelimit.AccountKey(subject)
	keys := []string{reset, ratelimit.LoginIPKey(req.ClientIP)}
	deviceID, known := s.auth.Devices.Verify(req.DeviceCookie, subject, now)
	if known {
		reset = ratelimit.DeviceKey(deviceID)
		keys = []string{reset}
	}
	s.logger.Debug().Str("subject", subject).Bool("known_device", known).Msg("login attempt")

	attempt, err := s.auth.Limiter.Reserve(now, keys...)
	if err != nil {
		return nil, LoginResult{}, err
	}
	// Frees the slots if Session.Login panics; a no-op once the attempt has ended.
	defer attempt.Release()

	pair, id, err := s.auth.Session.Login(ctx, enum.Password, subject, req.Password)
	if err != nil {
		if !errors.Is(err, iauth.ErrInvalidCredentials) {
			attempt.Release()
			return nil, LoginResult{}, err
		}
		if attempt.Fail(s.auth.Clock.Now()) {
			err = errors.Join(err, iauth.ErrRateLimitReached)
		}
		return nil, LoginResult{}, err
	}
	attempt.Release()
	s.auth.Limiter.Reset(reset)
	device, err := s.auth.Devices.Issue(subject, s.auth.Clock.Now())
	if err != nil {
		return nil, LoginResult{}, err
	}
	s.logger.Info().Str("subject", subject).Str("user_id", id.UserID).Msg("login success")
	return id, LoginResult{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		SessionID:    id.SessionID,
		DeviceCookie: device,
	}, nil
}

// GetPermissions returns all available permissions in the system.
func (s *Service) GetPermissions() []enum.Permission {
	return enum.All
}

// GetRoles returns all roles from storage.
func (s *Service) GetRoles(ctx context.Context) ([]*model.Role, error) {
	res, err := s.store.ListRoles(ctx, nil, storage.ListOptions{Limit: 0})
	if err != nil {
		return nil, err
	}
	return res.Items, nil
}

// Logout revokes a session.
func (s *Service) Logout(ctx context.Context, req LogoutRequest) error {
	if req.SessionID == "" {
		return nil
	}
	if err := s.auth.Session.Revoke(ctx, req.SessionID); err != nil {
		return err
	}

	s.logger.Debug().Str("session_id", req.SessionID).Msg("logout")
	return nil
}
