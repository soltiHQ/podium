package server

import (
	"errors"
	"time"
)

const (
	defaultShutdownTimeout = 15 * time.Second
)

// Config controls server behavior.
type Config struct {
	// ShutdownTimeout is the maximum time allowed for a graceful shutdown.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	return c
}

// ErrNegativeShutdownTimeout indicates a negative shutdown timeout; zero keeps
// the default.
var ErrNegativeShutdownTimeout = errors.New("server: negative shutdown_timeout")

// Validate reports a negative shutdown timeout (zero keeps the default).
func (c Config) Validate() error {
	if c.ShutdownTimeout < 0 {
		return ErrNegativeShutdownTimeout
	}
	return nil
}
