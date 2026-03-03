package wire

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
	defaultAudience      = "control-plane"
	defaultIssuer        = "solti"
	defaultAccessTTL     = 15 * time.Minute
	defaultRefreshTTL    = 7 * 24 * time.Hour
	defaultRateWindow    = 1 * time.Minute
	defaultRateAttempts  = 5
)

// Config configures the authentication subsystem.
type Config struct {
	JWTSecret     string        `yaml:"jwt_secret"`
	Audience      string        `yaml:"audience"`
	Issuer        string        `yaml:"issuer"`
	AccessTTL     time.Duration `yaml:"access_ttl"`
	RefreshTTL    time.Duration `yaml:"refresh_ttl"`
	RateWindow    time.Duration `yaml:"rate_window"`
	RateAttempts  int           `yaml:"rate_attempts"`
	RotateRefresh bool          `yaml:"rotate_refresh"`
}

func (c Config) withDefaults() Config {
	if c.Audience == "" {
		c.Audience = defaultAudience
	}
	if c.Issuer == "" {
		c.Issuer = defaultIssuer
	}
	if c.AccessTTL <= 0 {
		c.AccessTTL = defaultAccessTTL
	}
	if c.RefreshTTL <= 0 {
		c.RefreshTTL = defaultRefreshTTL
	}
	if c.RateWindow <= 0 {
		c.RateWindow = defaultRateWindow
	}
	if c.RateAttempts <= 0 {
		c.RateAttempts = defaultRateAttempts
	}
	return c
}

// Auth is a composition root for the authentication subsystem.
//
// It wires together:
//
//   - JWT issuer and verifier (HS256)
//   - Session service (login/refresh/revoke use cases)
//   - RBAC resolver
//   - Password auth provider
//   - Login rate limiter
//
// Auth does not implement business logic itself; it aggregates fully
// configured components ready for use by HTTP/transport layers.
type Auth struct {
	// Clock used by token issuance and verification.
	Clock token.Clock

	// Limiter tracks failed login attempts and enforces temporary blocking.
	Limiter *ratelimit.Limiter

	// Session provides login, refresh, and revoke operations.
	Session *session2.Service

	// Verifier validates incoming access tokens.
	Verifier *jwt.HSVerifier
}

// NewAuth constructs a fully wired authentication stack.
func NewAuth(store storage.Storage, cfg Config) *Auth {
	cfg = cfg.withDefaults()

	var (
		clock   = token.RealClock()
		secretb = []byte(cfg.JWTSecret)

		verifier = jwt.NewHSVerifier(cfg.Issuer, cfg.Audience, secretb, clock)
		issuerHS = jwt.NewHSIssuer(secretb, clock)
		resolver = rbac.NewResolver(store)

		sesCfg = session2.Config{
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
		Session: session2.New(
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
