package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/storage"
)

type RBACResolver interface {
	ResolveUserPermissions(ctx context.Context, u *model.User) ([]kind.Permission, error)
}

// Service provides the core business logic for authentication, session management, and RBAC permissions resolution.
type Service struct {
	store    storage.Storage
	issuer   token.Issuer
	verifier token.Verifier
	clock    token.Clock
	cfg      Config
	rbac     RBACResolver
}

// New creates a new session service.
func New(store storage.Storage, issuer token.Issuer, clk token.Clock, cfg Config, rbac RBACResolver) *Service {
	if clk == nil {
		clk = token.RealClock()
	}
	return &Service{
		store:  store,
		issuer: issuer,
		clock:  clk,
		cfg:    cfg,
		rbac:   rbac,
	}
}

// Login authenticates by (subject, password), creates a session, returns access+refresh.
func (s *Service) Login(ctx context.Context, subject, password string) (*TokenPair, *identity.Identity, error) {
	if s.store == nil || s.issuer == nil || s.rbac == nil {
		return nil, nil, ErrInvalidRequest
	}
	if subject == "" || password == "" {
		return nil, nil, ErrInvalidCredentials
	}

	u, err := s.store.GetUserBySubject(ctx, subject)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, ErrInvalidCredentials
		}
		return nil, nil, err
	}
	if u.Disabled() {
		return nil, nil, ErrInvalidCredentials
	}

	cred, err := s.store.GetCredentialByUserAndAuth(ctx, u.ID(), kind.Password)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, ErrInvalidCredentials
		}
		return nil, nil, err
	}

	if err = credentials.VerifyPassword(cred, password); err != nil {
		return nil, nil, ErrInvalidCredentials
	}

	perms, err := s.rbac.ResolveUserPermissions(ctx, u)
	if err != nil {
		// если RBAC говорит "нет прав" — наружу лучше как unauthorized/invalid
		return nil, nil, ErrUnauthorized
	}
	if len(perms) == 0 {
		return nil, nil, ErrUnauthorized
	}

	now := s.clock.Now()

	refreshRaw, refreshHash, err := newRefreshToken()
	if err != nil {
		return nil, nil, err
	}

	sessionID := newID16()
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
		TokenID:   newID16(),
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
// If RotateRefresh is enabled, rotates refresh token (hash stored, raw returned once).
func (s *Service) Refresh(ctx context.Context, sessionID, refreshRaw string) (*TokenPair, *identity.Identity, error) {
	if s.store == nil || s.issuer == nil || s.rbac == nil {
		return nil, nil, ErrInvalidRequest
	}
	if sessionID == "" || refreshRaw == "" {
		return nil, nil, ErrInvalidRefresh
	}

	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, nil, ErrInvalidRefresh
	}

	now := s.clock.Now()

	if sess.Revoked() {
		return nil, nil, ErrRevoked
	}
	if sess.Expired(now) {
		return nil, nil, ErrInvalidRefresh
	}

	inHash, err := hashRefreshToken(refreshRaw)
	if err != nil {
		return nil, nil, ErrInvalidRefresh
	}
	if !constantTimeEq(inHash, sess.RefreshHash()) {
		return nil, nil, ErrInvalidRefresh
	}

	// Recompute perms on refresh (keeps RBAC changes effective).
	u, err := s.store.GetUser(ctx, sess.UserID())
	if err != nil {
		return nil, nil, ErrInvalidRefresh
	}
	if u.Disabled() {
		return nil, nil, ErrInvalidRefresh
	}
	perms, err := s.rbac.ResolveUserPermissions(ctx, u)
	if err != nil || len(perms) == 0 {
		return nil, nil, ErrUnauthorized
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
		TokenID:   newID16(),
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
		return ErrInvalidRequest
	}
	if sessionID == "" {
		return ErrInvalidRequest
	}
	return s.store.RevokeSession(ctx, sessionID, s.clock.Now())
}

func newID16() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
