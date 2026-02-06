package server

import "time"

const (
	defaultShutdownTimeout = 15 * time.Second
)

// Config controls server behavior.
type Config struct {
	// ShutdownTimeout is the maximum time allowed for a graceful shutdown.
	ShutdownTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	return c
}
