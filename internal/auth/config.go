package auth

import "time"

const (
	defaultAudience     = "control-plane"
	defaultIssuer       = "solti"
	defaultAccessTTL    = 15 * time.Minute
	defaultRefreshTTL   = 7 * 24 * time.Hour
	defaultRateWindow   = 1 * time.Minute
	defaultRateAttempts = 5
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
