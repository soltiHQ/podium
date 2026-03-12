package sync

import "time"

const (
	defaultTickInterval = 10 * time.Second
	defaultPushTimeout  = 15 * time.Second

	defaultName           = "sync"
	defaultMaxRetries     = 5
	defaultMaxConcurrency = 4
)

// Config configures the sync runner.
type Config struct {
	TickInterval time.Duration `yaml:"tick_interval"`
	PushTimeout  time.Duration `yaml:"push_timeout"`

	MaxConcurrency int `yaml:"max_concurrency"`
	MaxRetries     int `yaml:"max_retries"`

	Name string `yaml:"name"`
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
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = defaultMaxConcurrency
	}
	return c
}
