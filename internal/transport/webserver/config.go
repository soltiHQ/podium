package webserver

import (
	"github.com/soltiHQ/control-plane/internal/transport/config"

	"github.com/rs/zerolog"
)

// Config represents the configuration for the api server.
type Config struct {
	configHTTP config.HttpConfig
	logLevel   zerolog.Level

	addrHTTP string
	devMode  bool
}

// NewConfig creates a new configuration instance.
func NewConfig(opts ...Option) Config {
	cfg := Config{
		configHTTP: config.NewHttpConfig(),
		logLevel:   zerolog.InfoLevel,
		devMode:    false,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
