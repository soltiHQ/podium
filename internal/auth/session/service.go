package session

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	passwordprovider "github.com/soltiHQ/control-plane/internal/auth/providers/password"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// RBACResolver defines the contract for resolving effective permissions.
type RBACResolver interface {
	ResolveUserPermissions(ctx context.Context, u *model.User) ([]enum.Permission, error)
}

// Service provides session and token business logic.
//
// Service authenticates a principal through a provider, enforces authorization
// via RBAC, persists a session record (refresh hash), and issues an access token.
type Service struct {
	store storage.Storage

	issuer token.Issuer
	clock  token.Clock

	cfg  Config
	rbac RBACResolver

	providers map[enum.Auth]providers.Provider
}

// New creates a new session service.
//
// If clk is nil, token.RealClock() is used.
// If provs is nil, an empty provider map is created.
func New(
	store storage.Storage,
	issuer token.Issuer,
	clk token.Clock,
	cfg Config,
	rbac RBACResolver,
	provs map[enum.Auth]providers.Provider,
) *Service {
	if clk == nil {
		clk = token.RealClock()
	}
	if provs == nil {
		provs = make(map[enum.Auth]providers.Provider, 4)
	}
	return &Service{
		store:     store,
		issuer:    issuer,
		clock:     clk,
		cfg:       cfg,
		rbac:      rbac,
		providers: provs,
	}
}

// ensureReady validates that the service is properly wired.
func (s *Service) ensureReady() error {
	if s == nil || s.store == nil || s.issuer == nil || s.rbac == nil || s.clock == nil {
		return auth.ErrInvalidRequest
	}
	return nil
}

// provider returns a provider for the given kind and validates its kind contract.
func (s *Service) provider(kind enum.Auth) (providers.Provider, error) {
	p := s.providers[kind]
	if p == nil {
		return nil, auth.ErrInvalidRequest
	}
	if p.Kind() != kind {
		return nil, auth.ErrInvalidRequest
	}
	return p, nil
}

// Login authenticates using the specified auth kind, creates a session,
// and returns access+refresh tokens along with the identity used for issuance.
//
// Contract:
//   - Authenticate is delegated to the configured provider for authKind.
//   - Authorization is enforced via RBAC: an empty effective permission set is denied.
//   - On success, only a hash of the refresh token is stored; raw refresh token is returned to caller.
//   - a Returned permission list in identity is the effective permission set.
//
// Errors:
//   - auth.ErrInvalidRequest for invalid wiring, unsupported auth kind, or invalid provider mapping.
//   - auth.ErrInvalidCredentials when subject/secret are empty (caller error).
//   - auth.ErrUnauthorized when RBAC denies access (no effective permissions or resolver error).
//   - Propagates provider and storage errors from dependencies.
func (s *Service) Login(ctx context.Context, authKind enum.Auth, subject, secret string) (*TokenPair, *identity.Identity, error) {
	if err := s.ensureReady(); err != nil {
		return nil, nil, err
	}
	if subject == "" || secret == "" {
		return nil, nil, auth.ErrInvalidCredentials
	}

	p, err := s.provider(authKind)
	if err != nil {
		return nil, nil, err
	}

	var req providers.Request
	switch authKind {
	case enum.Password:
		req = passwordprovider.Request{Subject: subject, Password: secret}
	default:
		return nil, nil, auth.ErrInvalidRequest
	}

	res, err := p.Authenticate(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	now := s.clock.Now()
	if res == nil || res.User == nil || res.Credential == nil {
		return nil, nil, auth.ErrInvalidRequest
	}

	u := res.User
	cred := res.Credential

	perms, err := s.rbac.ResolveUserPermissions(ctx, u)
	if err != nil || len(perms) == 0 {
		return nil, nil, auth.ErrUnauthorized
	}

	refreshRaw, refreshHash, err := newRefreshToken()
	if err != nil {
		return nil, nil, err
	}
	sessionID, err := newID16()
	if err != nil {
		return nil, nil, err
	}

	sess, err := model.NewSession(
		sessionID,
		u.ID(),
		cred.ID(),
		cred.AuthKind(),
		refreshHash,
		now.Add(s.cfg.RefreshTTL),
	)
	if err != nil {
		return nil, nil, err
	}
	if err = s.store.CreateSession(ctx, sess); err != nil {
		return nil, nil, err
	}

	tokenID, err := newID16()
	if err != nil {
		return nil, nil, err
	}
	id := &identity.Identity{
		IssuedAt:  now,
		NotBefore: now,
		ExpiresAt: now.Add(s.cfg.AccessTTL),

		Subject: u.Subject(),
		UserID:  u.ID(),
		Name:    u.Name(),
		Email:   u.Email(),

		Issuer:    s.cfg.Issuer,
		Audience:  []string{s.cfg.Audience},
		TokenID:   tokenID,
		SessionID: sessionID,

		Permissions: perms,
	}
	access, err := s.issuer.Issue(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: refreshRaw}, id, nil
}

// Refresh validates a refresh token against the stored session and issues a new access token.
//
// Contract:
//   - refresh token verification uses constant-time comparison.
//   - A revoked session is rejected with auth.ErrRevoked.
//   - An expired session or mismatched token is rejected with auth.ErrInvalidRefresh.
//   - Authorization is enforced via RBAC: empty effective permission set is denied.
//   - When cfg.RotateRefresh is enabled, refresh token is rotated and the stored hash+expiry are updated.
//
// Errors:
//   - auth.ErrInvalidRequest for invalid wiring.
//   - auth.ErrInvalidRefresh for malformed input or failed validation (no detail leakage).
//   - auth.ErrRevoked when the session is revoked.
//   - auth.ErrUnauthorized when RBAC denies access (no effective permissions or resolver error).
//   - Propagates storage and issuer errors from dependencies where applicable.
func (s *Service) Refresh(ctx context.Context, sessionID, refreshRaw string) (*TokenPair, *identity.Identity, error) {
	if err := s.ensureReady(); err != nil {
		return nil, nil, err
	}
	if sessionID == "" || refreshRaw == "" {
		return nil, nil, auth.ErrInvalidRefresh
	}

	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, nil, auth.ErrInvalidRefresh
	}

	now := s.clock.Now()
	if sess.Revoked() {
		return nil, nil, auth.ErrRevoked
	}
	if sess.Expired(now) {
		return nil, nil, auth.ErrInvalidRefresh
	}

	inHash, err := hashRefreshToken(refreshRaw)
	if err != nil {
		return nil, nil, auth.ErrInvalidRefresh
	}
	if subtle.ConstantTimeCompare(inHash, sess.RefreshHash()) != 1 {
		return nil, nil, auth.ErrInvalidRefresh
	}

	u, err := s.store.GetUser(ctx, sess.UserID())
	if err != nil {
		return nil, nil, auth.ErrInvalidRefresh
	}
	if u.Disabled() {
		return nil, nil, auth.ErrInvalidRefresh
	}

	perms, err := s.rbac.ResolveUserPermissions(ctx, u)
	if err != nil || len(perms) == 0 {
		return nil, nil, auth.ErrUnauthorized
	}

	outRefresh := refreshRaw
	if s.cfg.RotateRefresh {
		newRaw, newHash, err := newRefreshToken()
		if err != nil {
			return nil, nil, err
		}
		newExp := now.Add(s.cfg.RefreshTTL)
		if err := s.store.RotateRefresh(ctx, sessionID, newHash, newExp); err != nil {
			return nil, nil, err
		}
		outRefresh = newRaw
	}

	tokenID, err := newID16()
	if err != nil {
		return nil, nil, err
	}
	id := &identity.Identity{
		IssuedAt:  now,
		NotBefore: now,
		ExpiresAt: now.Add(s.cfg.AccessTTL),

		Subject: u.Subject(),
		UserID:  u.ID(),
		Name:    u.Name(),
		Email:   u.Email(),

		Issuer:    s.cfg.Issuer,
		Audience:  []string{s.cfg.Audience},
		TokenID:   tokenID,
		SessionID: sessionID,

		Permissions: perms,
	}
	access, err := s.issuer.Issue(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: outRefresh}, id, nil
}

// Revoke revokes a session by ID.
//
// Errors:
//   - auth.ErrInvalidRequest for invalid wiring or empty session ID.
//   - auth.ErrInvalidRequest if the session does not exist.
//   - Propagates storage errors for backend failures.
func (s *Service) Revoke(ctx context.Context, sessionID string) error {
	if err := s.ensureReady(); err != nil {
		return err
	}
	if sessionID == "" {
		return auth.ErrInvalidRequest
	}
	if err := s.store.RevokeSession(ctx, sessionID, s.clock.Now()); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return auth.ErrInvalidRequest
		}
		return err
	}
	return nil
}
