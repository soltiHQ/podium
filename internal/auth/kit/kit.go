// Package kit is the composition root for the authentication subsystem.
//
// It wires together independently-developed pieces (JWT issuance/verification,
// session service, RBAC resolver, password provider, login rate limiter) into
// a single [Auth] aggregate ready for use by HTTP/transport layers.
//
// Kit lives in a sub-package because providers under internal/auth/providers/*
// import the parent internal/auth package for sentinel errors; placing the
// composition root in the parent would form an import cycle.
package kit

import (
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	passwordprovider "github.com/soltiHQ/control-plane/internal/auth/providers/password"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Auth aggregates fully configured authentication components.
type Auth struct {
	// Clock used by token issuance and verification.
	Clock token.Clock

	// Limiter tracks failed login attempts and enforces temporary blocking.
	Limiter *ratelimit.Limiter

	// Session provides login, refresh, and revoke operations.
	Session *session.Service

	// Verifier validates incoming access tokens.
	Verifier *jwt.HSVerifier
}

// New constructs a fully wired authentication stack.
func New(store storage.Storage, cfg auth.Config) *Auth {
	cfg = cfg.WithDefaults()

	var (
		clock   = token.RealClock()
		secretb = []byte(cfg.JWTSecret)

		verifier = jwt.NewHSVerifier(cfg.Issuer, cfg.Audience, secretb, clock)
		issuerHS = jwt.NewHSIssuer(secretb, clock)
		resolver = rbac.NewResolver(store)

		sesCfg = session.Config{
			Audience:      cfg.Audience,
			Issuer:        cfg.Issuer,
			AccessTTL:     cfg.AccessTTL,
			RefreshTTL:    cfg.RefreshTTL,
			RotateRefresh: cfg.RotateRefresh,
		}
	)
	return &Auth{
		Clock:    clock,
		Verifier: verifier,
		Session: session.New(
			store,
			issuerHS,
			clock,
			sesCfg,
			resolver,
			map[kind.Auth]providers.Provider{
				kind.Password: passwordprovider.New(store),
			},
		),
		Limiter: ratelimit.New(ratelimit.Config{
			MaxAttempts: cfg.RateAttempts,
			BlockWindow: cfg.RateWindow,
		}),
	}
}
