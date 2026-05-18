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
	JWTSecret     string        `yaml:"jwt_secret"     envconfig:"JWT_SECRET"`
	Audience      string        `yaml:"audience"       envconfig:"AUDIENCE"`
	Issuer        string        `yaml:"issuer"         envconfig:"ISSUER"`
	AccessTTL     time.Duration `yaml:"access_ttl"     envconfig:"ACCESS_TTL"`
	RefreshTTL    time.Duration `yaml:"refresh_ttl"    envconfig:"REFRESH_TTL"`
	RateWindow    time.Duration `yaml:"rate_window"    envconfig:"RATE_WINDOW"`
	RateAttempts  int           `yaml:"rate_attempts"  envconfig:"RATE_ATTEMPTS"`
	RotateRefresh bool          `yaml:"rotate_refresh" envconfig:"ROTATE_REFRESH"`
}

// ErrJWTSecretTooShort indicates that JWTSecret is unset or shorter than
// [MinJWTSecretLen]. Caller MUST set SOLTI_AUTH_JWT_SECRET (or auth.jwt_secret
// in YAML) to a value of at least 32 characters.
var ErrJWTSecretTooShort = errors.New("auth: jwt_secret unset or shorter than minimum length")

// Validate reports an error if security-critical fields are unset or weak.
func (c Config) Validate() error {
	if len(c.JWTSecret) < MinJWTSecretLen {
		return fmt.Errorf("%w (min %d chars, set SOLTI_AUTH_JWT_SECRET)", ErrJWTSecretTooShort, MinJWTSecretLen)
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
