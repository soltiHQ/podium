package history

import (
	"fmt"
	"time"
)

type Config struct {
	TickInterval        time.Duration `yaml:"tick_interval" envconfig:"TICK_INTERVAL"`
	CycleTimeout        time.Duration `yaml:"cycle_timeout" envconfig:"CYCLE_TIMEOUT"`
	MaxConcurrency      int           `yaml:"max_concurrency" envconfig:"MAX_CONCURRENCY"`
	MaxAgentsPerCycle   int           `yaml:"max_agents_per_cycle" envconfig:"MAX_AGENTS_PER_CYCLE"`
	PageSize            int           `yaml:"page_size" envconfig:"PAGE_SIZE"`
	MaxRunContinuations int           `yaml:"max_run_continuations" envconfig:"MAX_RUN_CONTINUATIONS"`
	MaxPages            int           `yaml:"max_pages" envconfig:"MAX_PAGES"`
}

func DefaultConfig() Config {
	return Config{TickInterval: 5 * time.Second, CycleTimeout: 8 * time.Second, MaxConcurrency: 2, MaxAgentsPerCycle: 100, PageSize: 256, MaxPages: 4, MaxRunContinuations: 1024}
}

func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.TickInterval == 0 {
		c.TickInterval = d.TickInterval
	}
	if c.CycleTimeout == 0 {
		c.CycleTimeout = d.CycleTimeout
	}
	if c.MaxConcurrency == 0 {
		c.MaxConcurrency = d.MaxConcurrency
	}
	if c.MaxAgentsPerCycle == 0 {
		c.MaxAgentsPerCycle = d.MaxAgentsPerCycle
	}
	if c.PageSize == 0 {
		c.PageSize = d.PageSize
	}
	if c.MaxPages == 0 {
		c.MaxPages = d.MaxPages
	}
	if c.MaxRunContinuations == 0 {
		c.MaxRunContinuations = d.MaxRunContinuations
	}
	return c
}

func (c Config) Validate() error {
	c = c.WithDefaults()
	if c.TickInterval <= 0 || c.CycleTimeout <= 0 || c.MaxConcurrency < 1 || c.MaxConcurrency > 32 || c.MaxAgentsPerCycle < 1 || c.MaxAgentsPerCycle > storageAgentPageLimit || c.PageSize < 1 || c.PageSize > 1000 || c.MaxPages < 1 || c.MaxPages > 64 || c.MaxRunContinuations < 1 || c.MaxRunContinuations > 10000 {
		return fmt.Errorf("history collector: durations/counts must be positive, concurrency<=32, agents<=500, page_size<=1000 and max_pages<=64 and max_run_continuations<=10000")
	}
	return nil
}

const storageAgentPageLimit = 500
