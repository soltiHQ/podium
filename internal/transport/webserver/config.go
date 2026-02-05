package webserver

import (
	"github.com/soltiHQ/control-plane/internal/transport/config"

	"github.com/rs/zerolog"
)

// Config represents the configuration for the web server.
type Config struct {
	configHTTP config.HttpConfig
	logLevel   zerolog.Level

	addrHTTP string
}

// NewConfig creates a new configuration instance.
func NewConfig(opts ...Option) Config {
	cfg := Config{
		configHTTP: config.NewHttpConfig(),
		logLevel:   zerolog.InfoLevel,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
