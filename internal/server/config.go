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
	// ShutdownDelay is how long podium waits between the first shutdown signal,
	// which turns readiness off, and stopping its listeners: the time a load
	// balancer needs to stop sending requests. Zero, the default, waits not at
	// all. It is not part of ShutdownTimeout.
	ShutdownDelay time.Duration `yaml:"shutdown_delay"`

	// ShutdownTimeout is the maximum time allowed for a graceful shutdown: the
	// listeners stop accepting, and the requests in flight finish within it.
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

// ErrNegativeShutdownDelay indicates a negative shutdown delay; zero waits not at
// all.
var ErrNegativeShutdownDelay = errors.New("server: negative shutdown_delay")

// Validate reports a negative shutdown timeout (zero keeps the default) or a
// negative shutdown delay.
func (c Config) Validate() error {
	if c.ShutdownDelay < 0 {
		return ErrNegativeShutdownDelay
	}
	if c.ShutdownTimeout < 0 {
		return ErrNegativeShutdownTimeout
	}
	return nil
}
