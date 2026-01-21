package edgeserver

import (
	"github.com/soltiHQ/control-plane/internal/transport/grpcconfig"
	"github.com/soltiHQ/control-plane/internal/transport/httpconfig"

	"github.com/rs/zerolog"
)

type Config struct {
	addrHTTP string
	addrGRPC string

	configHTTP httpconfig.Config
	configGRPC grpcconfig.Config

	logLevel zerolog.Level
}

func NewConfig(opts ...Option) Config {
	cfg := Config{
		configHTTP: httpconfig.New(),
		configGRPC: grpcconfig.New(),
		logLevel:   zerolog.InfoLevel,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
