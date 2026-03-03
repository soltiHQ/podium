package lifecycle

import "time"

const (
	defaultTickInterval = 10 * time.Second
	defaultHeartbeat    = 30 * time.Second
	
	defaultInactiveMultiplier   = 2
	defaultDisconnectMultiplier = 5
	defaultDeleteMultiplier     = 10
)

// Config configures the lifecycle runner.
type Config struct {
	TickInterval         time.Duration `yaml:"tick_interval"`
	DefaultHeartbeat     time.Duration `yaml:"default_heartbeat"`
	InactiveMultiplier   int           `yaml:"inactive_multiplier"`
	DisconnectMultiplier int           `yaml:"disconnect_multiplier"`
	DeleteMultiplier     int           `yaml:"delete_multiplier"`
	Name                 string        `yaml:"name"`
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
	if c.DisconnectMultiplier <= c.InactiveMultiplier {
		c.DisconnectMultiplier = c.InactiveMultiplier + 1
	}
	if c.DeleteMultiplier <= c.DisconnectMultiplier {
		c.DeleteMultiplier = c.DisconnectMultiplier + 1
	}
	return c
}
