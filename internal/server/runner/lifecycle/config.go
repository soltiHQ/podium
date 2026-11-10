package lifecycle

import (
	"errors"
	"fmt"
	"time"
)

const (
	defaultTickInterval = 10 * time.Second
	defaultHeartbeat    = 30 * time.Second
	defaultDeleteAfter  = 24 * time.Hour

	defaultName           = "lifecycle"
	defaultMaxConcurrency = 4

	defaultInactiveMultiplier   = 2
	defaultDisconnectMultiplier = 5
	defaultDeleteMultiplier     = 10
)

// Config configures the lifecycle runner.
//
// An agent is deleted once its silence exceeds the larger of DeleteAfter and DeleteMultiplier × its heartbeat interval.
type Config struct {
	TickInterval         time.Duration `yaml:"tick_interval"`
	DefaultHeartbeat     time.Duration `yaml:"default_heartbeat"`
	InactiveMultiplier   int           `yaml:"inactive_multiplier"`
	DisconnectMultiplier int           `yaml:"disconnect_multiplier"`
	DeleteMultiplier     int           `yaml:"delete_multiplier"`
	DeleteAfter          time.Duration `yaml:"delete_after"`
	MaxConcurrency       int           `yaml:"max_concurrency"`
	Name                 string        `yaml:"name"`
}

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = defaultName
	}
	if c.TickInterval <= 0 {
		c.TickInterval = defaultTickInterval
	}
	if c.InactiveMultiplier <= 0 {
		c.InactiveMultiplier = defaultInactiveMultiplier
	}
	if c.DisconnectMultiplier <= 0 {
		c.DisconnectMultiplier = defaultDisconnectMultiplier
	}
	if c.DeleteMultiplier <= 0 {
		c.DeleteMultiplier = defaultDeleteMultiplier
	}
	if c.DeleteAfter <= 0 {
		c.DeleteAfter = defaultDeleteAfter
	}
	if c.DefaultHeartbeat <= 0 {
		c.DefaultHeartbeat = defaultHeartbeat
	}
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = defaultMaxConcurrency
	}
	if c.DisconnectMultiplier <= c.InactiveMultiplier {
		c.DisconnectMultiplier = c.InactiveMultiplier + 1
	}
	if c.DeleteMultiplier <= c.DisconnectMultiplier {
		c.DeleteMultiplier = c.DisconnectMultiplier + 1
	}
	return c
}

// ErrNegativeValue indicates a negative interval, multiplier or count; zero
// keeps the default.
var ErrNegativeValue = errors.New("lifecycle: negative value")

// Validate reports a negative interval, multiplier or count (zero keeps the
// default).
func (c Config) Validate() error {
	for _, v := range []struct {
		key string
		neg bool
	}{
		{"tick_interval", c.TickInterval < 0},
		{"default_heartbeat", c.DefaultHeartbeat < 0},
		{"inactive_multiplier", c.InactiveMultiplier < 0},
		{"disconnect_multiplier", c.DisconnectMultiplier < 0},
		{"delete_multiplier", c.DeleteMultiplier < 0},
		{"delete_after", c.DeleteAfter < 0},
		{"max_concurrency", c.MaxConcurrency < 0},
	} {
		if v.neg {
			return fmt.Errorf("%w: %s", ErrNegativeValue, v.key)
		}
	}
	return nil
}
