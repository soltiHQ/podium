package auth

import (
	"errors"
	"fmt"
	"time"
)

const (
	defaultAudience     = "control-plane"
	defaultIssuer       = "solti"
	defaultAccessTTL    = 15 * time.Minute
	defaultRefreshTTL   = 7 * 24 * time.Hour
	defaultRateWindow   = 1 * time.Minute
	defaultRateAttempts = 5

	// MinJWTSecretLen is the minimum acceptable JWT-signing-secret length.
	// HMAC-SHA256 keys shorter than 32 bytes weaken the signature.
	MinJWTSecretLen = 32
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

	// CookieSecure decides when the auth and device cookies carry the Secure
	// attribute: CookieSecureAuto (the default) or CookieSecureAlways.
	CookieSecure string `yaml:"cookie_secure"`
}

// Values of [Config.CookieSecure].
const (
	// CookieSecureAuto marks the cookies Secure when the client used
	// HTTPS: a TLS connection, or X-Forwarded-Proto https from a trusted proxy.
	CookieSecureAuto = "auto"
	// CookieSecureAlways marks the cookies Secure on every response.
	CookieSecureAlways = "always"
)

// ErrJWTSecretTooShort indicates that JWTSecret is unset or shorter than
// [MinJWTSecretLen]. Caller MUST set SOLTI_AUTH_JWT_SECRET (or auth.jwt_secret
// in YAML) to a value of at least 32 characters.
var ErrJWTSecretTooShort = errors.New("auth: jwt_secret unset or shorter than minimum length")

// ErrInvalidCookieSecure indicates a cookie_secure value other than
// [CookieSecureAuto] or [CookieSecureAlways].
var ErrInvalidCookieSecure = errors.New(`auth: cookie_secure must be "auto" or "always"`)

// Validate reports an error if security-critical fields are unset or weak.
func (c Config) Validate() error {
	if len(c.JWTSecret) < MinJWTSecretLen {
		return fmt.Errorf("%w (min %d chars, set SOLTI_AUTH_JWT_SECRET)", ErrJWTSecretTooShort, MinJWTSecretLen)
	}
	if c.CookieSecure != CookieSecureAuto && c.CookieSecure != CookieSecureAlways {
		return fmt.Errorf("%w, got %q", ErrInvalidCookieSecure, c.CookieSecure)
	}
	return nil
}

// WithDefaults returns a copy of the config with zero-valued fields
// filled with safe defaults.
func (c Config) WithDefaults() Config {
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
