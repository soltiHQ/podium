// Package config aggregates per-package configuration structs and
// provides Load / Default constructors:
//   - Load reads: defaults → YAML file → ENV overrides (SOLTI_ prefix).
//   - Default returns safe development defaults without external sources.
package config

import (
	"flag"
	"fmt"
	"os"

	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v3"

	"github.com/soltiHQ/control-plane/internal/auth/wire"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/grpcserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/lifecycle"
	syncrunner "github.com/soltiHQ/control-plane/internal/server/runner/sync"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
)

const (
	envPrefix = "SOLTI"
)

// Config holds the full application configuration.
type Config struct {
	HTTP          httpserver.Config     `yaml:"http"           envconfig:"HTTP"`
	HTTPDiscovery httpserver.Config     `yaml:"http_discovery" envconfig:"HTTP_DISCOVERY"`
	GRPC          grpcserver.Config     `yaml:"grpc"           envconfig:"GRPC"`
	Sync          syncrunner.Config     `yaml:"sync"           envconfig:"SYNC"`
	Lifecycle     lifecycle.Config      `yaml:"lifecycle"      envconfig:"LIFECYCLE"`
	Triggers      htmx.Config           `yaml:"triggers"       envconfig:"TRIGGERS"`
	Server        server.Config         `yaml:"server"         envconfig:"SERVER"`
	Auth          wire.Config           `yaml:"auth"           envconfig:"AUTH"`
	CORS          middleware.CORSConfig `yaml:"cors"           envconfig:"CORS"`
	Cluster       cluster.Config        `yaml:"cluster"        envconfig:"CLUSTER"`
}

// Default returns the default development configuration.
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
		Cluster: cluster.DefaultConfig(),
	}
}

// Load reads configuration in priority order: defaults → YAML file → ENV.
func Load() (Config, error) {
	var (
		path = configPath()
		cfg  = Default()
	)
	if path != "" {
		if err := loadYAML(path, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
	}
	if err := envconfig.Process(envPrefix, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: env: %w", err)
	}
	return cfg, nil
}

// configPath returns the config file path from --config flag or CONFIG_PATH env.
func configPath() string {
	var path string
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.StringVar(&path, "config", "", "path to YAML config file")
	_ = fs.Parse(os.Args[1:])

	if path != "" {
		return path
	}
	return os.Getenv("CONFIG_PATH")
}

// loadYAML reads and unmarshals a YAML file into dst.
func loadYAML(path string, dst *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if err = yaml.NewDecoder(f).Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
