package svc

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	passwordprovider "github.com/soltiHQ/control-plane/internal/auth/providers/password"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	session2 "github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/storage"
)

const (
	audience = "control-plane"
	issuer   = "solti"
)

type Auth struct {
	Clock token.Clock

	Limiter  *ratelimit.Limiter
	Session  *session2.Service
	Verifier *jwt.HSVerifier
}

// NewAuth creates a new auth service.
func NewAuth(storage storage.Storage, secret string, aTTL, rTTL, wTTL time.Duration, attemptLimit int) *Auth {
	var (
		clock   = token.RealClock()
		secretb = []byte(secret)

		verifier = jwt.NewHSVerifier(issuer, audience, secretb, clock)
		issuerHS = jwt.NewHSIssuer(secretb, clock)
		resolver = rbac.NewResolver(storage)

		sesCfg = session2.Config{
			Audience:      audience,
			Issuer:        issuer,
			AccessTTL:     aTTL,
			RefreshTTL:    rTTL,
			RotateRefresh: true,
		}
	)
	return &Auth{
		Clock:    clock,
		Verifier: verifier,
		Session: session2.New(
			storage,
			issuerHS,
			clock,
			sesCfg,
			resolver,
			map[kind.Auth]providers.Provider{
				kind.Password: passwordprovider.New(storage),
			},
		),
		Limiter: ratelimit.New(ratelimit.Config{
			MaxAttempts: attemptLimit,
			BlockWindow: wTTL,
		}),
	}
}
