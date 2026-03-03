// Package config aggregates per-package configuration structs.
// Provides a single Default() constructor for development use.
package config

import (
	"github.com/soltiHQ/control-plane/internal/auth/wire"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/grpcserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/lifecycle"
	syncrunner "github.com/soltiHQ/control-plane/internal/server/runner/sync"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/uikit/trigger"
)

// Config holds the full application configuration.
// Each field corresponds to a per-package Config struct that owns its own defaults via withDefaults().
type Config struct {
	HTTP          httpserver.Config
	HTTPDiscovery httpserver.Config
	GRPC          grpcserver.Config
	Sync          syncrunner.Config
	Lifecycle     lifecycle.Config
	Triggers      trigger.Config
	Server        server.Config
	Auth          wire.Config
	CORS          middleware.CORSConfig
}

// Default returns the default development configuration.
// Zero-valued sub-configs inherit package-level defaults.
func Default() Config {
	return Config{
		HTTP:          httpserver.Config{Name: "http", Addr: ":8080"},
		HTTPDiscovery: httpserver.Config{Name: "http-discovery", Addr: ":8082"},
		GRPC:          grpcserver.Config{Name: "grpc-discovery", Addr: ":50051"},
		Auth: wire.Config{
			JWTSecret: "solti-fkhk5qo48thkads-85gnsdAdtXZvo9r",
		},
		CORS: middleware.CORSConfig{
			AllowOrigins: []string{"*"},
		},
	}
}
