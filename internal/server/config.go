package server

import "time"

const (
	defaultShutdownTimeout = 15 * time.Second
)

// Config controls server behavior.
type Config struct {
	// ShutdownTimeout is the maximum time allowed for a graceful shutdown.
	// If <= 0, defaultShutdownTimeout is used.
	ShutdownTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	return c
}
