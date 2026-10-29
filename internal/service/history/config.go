package history

import (
	"fmt"
	"time"
)

// Config bounds retained terminal observations, independently of SDK retention.
// MaxBytes charges ExecutionRecord.SizeBytes; it is not disk or RSS accounting.
type Config struct {
	Retention  time.Duration `yaml:"retention" envconfig:"RETENTION"`
	MaxRecords int           `yaml:"max_records" envconfig:"MAX_RECORDS"`
	MaxBytes   int64         `yaml:"max_bytes" envconfig:"MAX_BYTES"`
}

func DefaultConfig() Config {
	return Config{Retention: 7 * 24 * time.Hour, MaxRecords: 10000, MaxBytes: 32 * 1024 * 1024}
}

func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.Retention == 0 {
		c.Retention = d.Retention
	}
	if c.MaxRecords == 0 {
		c.MaxRecords = d.MaxRecords
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = d.MaxBytes
	}
	return c
}

func (c Config) Validate() error {
	c = c.WithDefaults()
	if c.Retention <= 0 || c.MaxRecords <= 0 || c.MaxBytes <= 0 {
		return fmt.Errorf("history: retention, max_records and max_bytes must be positive")
	}
	return nil
}
