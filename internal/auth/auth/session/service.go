package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	passwordprovider "github.com/soltiHQ/control-plane/internal/auth/providers/password"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// RBACResolver defines the contract for resolving effective permissions
// of an authenticated user according to the system's RBAC policy.
//
// Implementations are responsible for computing the union of:
//   - permissions granted via roles associated with the user
//   - permissions directly assigned to the user
//
// The resolver encapsulates authorization policy and may apply additional rules.
//
// Contract:
//   - Must return a non-nil error if the user has no effective permissions or if resolution fails.
//   - Must not perform authentication, token issuance, or session management.
//   - Must return a deduplicated set of permissions.
//   - Must not mutate the provided user instance.
type RBACResolver interface {
	ResolveUserPermissions(ctx context.Context, u *model.User) ([]kind.Permission, error)
}

// Service provides the core business logic.
type Service struct {
	store storage.Storage

	issuer token.Issuer
	clock  token.Clock

	cfg  Config
	rbac RBACResolver

	providers map[kind.Auth]providers.Provider
}

// New creates a new session service.
func New(
	store storage.Storage,
	issuer token.Issuer,
	clk token.Clock,
	cfg Config,
	rbac RBACResolver,
	provs map[kind.Auth]providers.Provider,
) *Service {
	if clk == nil {
		clk = token.RealClock()
	}
	if provs == nil {
		provs = make(map[kind.Auth]providers.Provider, 4)
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

// Login authenticates by (subject, password), creates a session, returns access+refresh.
func (s *Service) Login(ctx context.Context, subject, password string) (*TokenPair, *identity.Identity, error) {
	if s.store == nil || s.issuer == nil || s.rbac == nil {
		return nil, nil, auth.ErrInvalidRequest
	}
	p := s.providers[kind.Password]
	if p == nil {
		return nil, nil, auth.ErrInvalidRequest
	}

	res, err := p.Authenticate(ctx, passwordprovider.Request{
		Subject:  subject,
		Password: password,
	})
	if err != nil {
		return nil, nil, err
	}

	var (
		u    = res.User
		cred = res.Credential
		now  = s.clock.Now()
	)
	if u == nil || cred == nil {
		return nil, nil, auth.ErrInvalidRequest
	}

	perms, err := s.rbac.ResolveUserPermissions(ctx, u)
	if err != nil {
		return nil, nil, auth.ErrUnauthorized
	}
	if len(perms) == 0 {
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

// Refresh validates refresh token against stored session and issues a new access token.
func (s *Service) Refresh(ctx context.Context, sessionID, refreshRaw string) (*TokenPair, *identity.Identity, error) {
	if s.store == nil || s.issuer == nil || s.rbac == nil {
		return nil, nil, auth.ErrInvalidRequest
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
	if err != nil {
		return nil, nil, auth.ErrUnauthorized
	}
	if len(perms) == 0 {
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

	tokenId, err := newID16()
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
		TokenID:   tokenId,
		SessionID: sessionID,

		Permissions: perms,
	}
	access, err := s.issuer.Issue(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: outRefresh}, id, nil
}

func (s *Service) Revoke(ctx context.Context, sessionID string) error {
	if s.store == nil {
		return auth.ErrInvalidRequest
	}
	if sessionID == "" {
		return auth.ErrInvalidRequest
	}
	return s.store.RevokeSession(ctx, sessionID, s.clock.Now())
}

func newID16() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
