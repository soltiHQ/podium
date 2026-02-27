package sync

import "time"

const (
	defaultTickInterval = 10 * time.Second
	defaultPushTimeout  = 15 * time.Second

	defaultName       = "sync"
	defaultMaxRetries = 5
)

// Config configures the sync runner.
type Config struct {
	TickInterval time.Duration
	PushTimeout  time.Duration
	Name         string
	MaxRetries   int
}

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = defaultName
	}
	if c.TickInterval <= 0 {
		c.TickInterval = defaultTickInterval
	}
	if c.PushTimeout <= 0 {
		c.PushTimeout = defaultPushTimeout
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = defaultMaxRetries
	}
	return c
}
