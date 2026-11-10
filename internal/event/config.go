package event

import (
	"errors"
	"time"
)

// DefaultCoalesce is the default of [Config.Coalesce].
const DefaultCoalesce = time.Second

// ErrNegativeCoalesce indicates a negative [Config.Coalesce].
var ErrNegativeCoalesce = errors.New("event: coalesce must not be negative")

// Config configures the event hub. Zero values keep the defaults.
type Config struct {
	// Coalesce is the window of [WithCoalesce]: a refresh pulse of one kind
	// reaches the clients at most once per window. Zero keeps DefaultCoalesce; a
	// negative value is invalid.
	Coalesce time.Duration `yaml:"coalesce"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config { return Config{Coalesce: DefaultCoalesce} }

// WithDefaults replaces the zero values with the defaults.
func (c Config) WithDefaults() Config {
	if c.Coalesce == 0 {
		c.Coalesce = DefaultCoalesce
	}
	return c
}

// Validate reports a negative window.
func (c Config) Validate() error {
	if c.Coalesce < 0 {
		return ErrNegativeCoalesce
	}
	return nil
}
