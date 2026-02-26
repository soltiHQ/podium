package lifecycle

import "time"

const (
	defaultTickInterval         = 30 * time.Second
	defaultInactiveMultiplier   = 2
	defaultDisconnectMultiplier = 5
	defaultDeleteMultiplier     = 10
	defaultHeartbeat            = 30 * time.Second
)

// Config configures the lifecycle runner.
type Config struct {
	Name                 string
	TickInterval         time.Duration
	InactiveMultiplier   int
	DisconnectMultiplier int
	DeleteMultiplier     int
	DefaultHeartbeat     time.Duration
}

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = "lifecycle"
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
	if c.DefaultHeartbeat <= 0 {
		c.DefaultHeartbeat = defaultHeartbeat
	}
	return c
}
