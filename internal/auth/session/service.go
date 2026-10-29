package session

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

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
//   - auth.ErrInvalidRefresh for malformed input or failed validation, including
//     a session or user that is not found or a disabled user (no detail leakage).
//   - auth.ErrRevoked when the session is revoked.
//   - auth.ErrUnauthorized when the user has no effective permissions.
//   - Storage, RBAC resolver and issuer errors as they are: a failed lookup is
//     not a verdict on the refresh token.
func (s *Service) Refresh(ctx context.Context, sessionID, refreshRaw string) (*TokenPair, *identity.Identity, error) {
	if err := s.ensureReady(); err != nil {
		return nil, nil, err
	}
	if sessionID == "" || refreshRaw == "" {
		return nil, nil, auth.ErrInvalidRefresh
	}
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, nil, missingAs(err, auth.ErrInvalidRefresh)
	}
	if sess.Revoked() {
		return nil, nil, auth.ErrRevoked
	}
	if sess.Expired(s.clock.Now()) {
		return nil, nil, auth.ErrInvalidRefresh
	}
	inHash, err := hashRefreshToken(refreshRaw)
	if err != nil || subtle.ConstantTimeCompare(inHash, sess.RefreshHash()) != 1 {
		return nil, nil, auth.ErrInvalidRefresh
	}
	user, err := s.store.GetUser(ctx, sess.UserID())
	if err != nil {
		return nil, nil, missingAs(err, auth.ErrInvalidRefresh)
	}
	if user.Disabled() {
		return nil, nil, auth.ErrInvalidRefresh
	}
	// The RBAC resolver uses the outer store. Resolve before admission, then
	// revalidate the session and user immediately before signing/rotation.
	perms, err := s.rbac.ResolveUserPermissions(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	if len(perms) == 0 {
		return nil, nil, auth.ErrUnauthorized
	}
	outRefresh := refreshRaw
	var newHash []byte
	if s.cfg.RotateRefresh {
		outRefresh, newHash, err = newRefreshToken()
		if err != nil {
			return nil, nil, err
		}
	}
	tokenID, err := newID16()
	if err != nil {
		return nil, nil, err
	}
	var pair *TokenPair
	var id *identity.Identity
	issueCurrent := func(tx storage.Storage) error {
		current, err := tx.GetSession(ctx, sessionID)
		if err != nil {
			return missingAs(err, auth.ErrInvalidRefresh)
		}
		if current.Revoked() {
			return auth.ErrRevoked
		}
		// Bind the pre-resolved permissions to the same session principal. A
		// deleted/recreated session must not inherit another principal's resolution.
		if current.UserID() != sess.UserID() || current.CredentialID() != sess.CredentialID() || current.AuthKind() != sess.AuthKind() || subtle.ConstantTimeCompare(inHash, current.RefreshHash()) != 1 {
			return auth.ErrInvalidRefresh
		}
		now := s.clock.Now()
		if current.Expired(now) {
			return auth.ErrInvalidRefresh
		}
		currentUser, err := tx.GetUser(ctx, current.UserID())
		if err != nil {
			return missingAs(err, auth.ErrInvalidRefresh)
		}
		if currentUser.Disabled() {
			return auth.ErrInvalidRefresh
		}
		var rotation storage.ClockedSessionRotation
		if s.cfg.RotateRefresh {
			var ok bool
			rotation, ok = tx.(storage.ClockedSessionRotation)
			if !ok {
				return fmt.Errorf("%w: explicit session rotation timestamps are unsupported", storage.ErrInternal)
			}
		}
		id = &identity.Identity{
			IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(s.cfg.AccessTTL),
			Subject: currentUser.Subject(), UserID: currentUser.ID(), Name: currentUser.Name(), Email: currentUser.Email(),
			Issuer: s.cfg.Issuer, Audience: []string{s.cfg.Audience}, TokenID: tokenID, SessionID: sessionID, Permissions: perms,
		}
		// For rotation, signing stays inside the transaction so issuer failure cannot
		// consume the refresh token. Issuers sign locally and must not reenter storage.
		access, err := s.issuer.Issue(ctx, id)
		if err != nil {
			return err
		}
		if rotation != nil {
			if err := rotation.RotateRefreshAt(ctx, sessionID, newHash, now.Add(s.cfg.RefreshTTL), now); err != nil {
				return err
			}
		}
		pair = &TokenPair{AccessToken: access, RefreshToken: outRefresh}
		return nil
	}
	if s.cfg.RotateRefresh {
		// Rotation validates and consumes the old hash under one storage transaction.
		err = s.store.WithTx(ctx, issueCurrent)
	} else {
		// Without a mutation, retain read-only refresh instead of rewriting the
		// complete durable snapshot. HTTP admission rechecks current authorization.
		err = issueCurrent(s.store)
	}
	if err != nil {
		return nil, nil, err
	}
	return pair, id, nil
}

// missingAs returns verdict when a lookup found nothing or was given an
// invalid ID, and any other storage error, such as an outage, as it is.
func missingAs(err, verdict error) error {
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalidArgument) {
		return verdict
	}
	return err
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
