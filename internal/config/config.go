// Package config aggregates per-package configuration structs and
// provides Load / Default constructors:
//   - Load reads: defaults → YAML file → ENV overrides (SOLTI_ prefix).
//   - Default returns safe development defaults without external sources.
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v3"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/grpcserver"
	historyrunner "github.com/soltiHQ/control-plane/internal/server/runner/history"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/lifecycle"
	syncrunner "github.com/soltiHQ/control-plane/internal/server/runner/sync"
	"github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/tlsconf"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
)

const (
	envPrefix = "SOLTI"
)

// TLSConfig groups the control-plane's TLS settings: the server identity used by
// the HTTP and gRPC listeners, and the client identity used when dialing agents.
// Both are opt-in — empty means plaintext.
type TLSConfig struct {
	Server tlsconf.ServerConfig `yaml:"server" envconfig:"SERVER"`
	Client tlsconf.ClientConfig `yaml:"client" envconfig:"CLIENT"`
}

// AgentAuthConfig controls bearer-token authentication between agents and the
// control plane — one switch for both directions.
//
// When Require is true (the default), enrolled agents are authenticated: the
// first sync of an unknown agent ID enrolls its token (trust on first use) and
// sets the endpoint podium calls, every later sync must present that token,
// and outbound proxy calls present it. It does not keep other hosts from
// enrolling new agent IDs: until enrollment tokens exist, the discovery
// listeners must be reachable only from agent networks or require client
// certificates, and startup warns when no client CA is set. When false, no
// token is verified inbound and none is sent outbound, and startup logs a
// warning. Orthogonal to TLS.
type AgentAuthConfig struct {
	Require bool `yaml:"require" envconfig:"REQUIRE"`
}

// HistoryConfig separates durable retention from bounded collection.
type HistoryConfig struct {
	Archive   history.Config       `yaml:"archive" envconfig:"ARCHIVE"`
	Collector historyrunner.Config `yaml:"collector" envconfig:"COLLECTOR"`
}

// Config holds the full application configuration.
type Config struct {
	History       HistoryConfig            `yaml:"history" envconfig:"HISTORY"`
	HTTP          httpserver.Config        `yaml:"http"           envconfig:"HTTP"`
	HTTPDiscovery httpserver.Config        `yaml:"http_discovery" envconfig:"HTTP_DISCOVERY"`
	GRPC          grpcserver.Config        `yaml:"grpc"           envconfig:"GRPC"`
	Sync          syncrunner.Config        `yaml:"sync"           envconfig:"SYNC"`
	Lifecycle     lifecycle.Config         `yaml:"lifecycle"      envconfig:"LIFECYCLE"`
	Triggers      htmx.Config              `yaml:"triggers"       envconfig:"TRIGGERS"`
	Server        server.Config            `yaml:"server"         envconfig:"SERVER"`
	Auth          auth.Config              `yaml:"auth"           envconfig:"AUTH"`
	CORS          middleware.CORSConfig    `yaml:"cors"           envconfig:"CORS"`
	Cluster       cluster.Config           `yaml:"cluster"        envconfig:"CLUSTER"`
	Streams       middleware.StreamsConfig `yaml:"streams"        envconfig:"STREAMS"`
	TLS           TLSConfig                `yaml:"tls"            envconfig:"TLS"`
	AgentAuth     AgentAuthConfig          `yaml:"agent_auth"     envconfig:"AGENT_AUTH"`

	// TrustedProxies lists the reverse proxies (IP addresses or CIDR prefixes)
	// whose X-Forwarded-For and X-Forwarded-Proto headers are believed.
	// Empty by default: the TCP peer is the client.
	TrustedProxies []string `yaml:"trusted_proxies" envconfig:"TRUSTED_PROXIES"`

	// BootstrapAdminPassword is the initial password of the administrator the
	// first start creates (env SOLTI_BOOTSTRAP_ADMIN_PASSWORD). It is never read
	// from YAML, so config files stay free of it. Empty means bootstrap
	// generates a password.
	BootstrapAdminPassword string `yaml:"-" envconfig:"BOOTSTRAP_ADMIN_PASSWORD"`
}

// Default returns the default development configuration.
//
// Security-sensitive fields (e.g. [auth.Config.JWTSecret]) are intentionally
// left empty here: they must come from the environment (SOLTI_AUTH_JWT_SECRET)
// or YAML at load time. [Load] enforces this via [Config.Validate].
func Default() Config {
	return Config{
		HTTP:          httpserver.Config{Name: "http", Addr: ":8080"},
		HTTPDiscovery: httpserver.Config{Name: "http-discovery", Addr: ":8082"},
		GRPC:          grpcserver.Config{Name: "grpc-discovery", Addr: ":50051"},
		Auth:          auth.Config{CookieSecure: auth.CookieSecureAuto}, // JWTSecret must be supplied externally
		Cluster:       cluster.DefaultConfig(),
		History:       HistoryConfig{Archive: history.DefaultConfig(), Collector: historyrunner.DefaultConfig()},
		Streams: middleware.StreamsConfig{
			MaxPerIP:         10,
			SubscriberBuffer: 64,
		},
		AgentAuth: AgentAuthConfig{Require: true},
	}
}

// Validate reports an error if any sub-config has invalid or unsafe values.
func (c Config) Validate() error {
	if err := c.History.Archive.Validate(); err != nil {
		return fmt.Errorf("config.history.archive: %w", err)
	}
	if err := c.History.Collector.Validate(); err != nil {
		return fmt.Errorf("config.history.collector: %w", err)
	}
	if err := c.Cluster.Validate(); err != nil {
		return fmt.Errorf("config.cluster: %w", err)
	}
	if err := c.Auth.Validate(); err != nil {
		return fmt.Errorf("config.auth: %w", err)
	}
	if err := c.CORS.Validate(); err != nil {
		return fmt.Errorf("config.cors: %w", err)
	}
	if err := c.TLS.Server.Validate(); err != nil {
		return fmt.Errorf("config.tls.server: %w", err)
	}
	if err := c.TLS.Client.Validate(); err != nil {
		return fmt.Errorf("config.tls.client: %w", err)
	}
	if _, err := clientip.New(c.TrustedProxies); err != nil {
		return fmt.Errorf("config.trusted_proxies: %w", err)
	}
	return nil
}

// Load reads configuration in priority order: defaults → YAML file → ENV.
// The YAML file comes from the --config flag or CONFIG_PATH.
// Refuses to return a Config that fails [Config.Validate].
func Load() (Config, error) {
	return load(os.Args[1:])
}

// load is [Load] with the command-line arguments passed in.
func load(args []string) (Config, error) {
	var (
		path = configPath(args)
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
	// The env value is split on commas only, so "10.0.0.1, 10.0.0.2" leaves a space.
	for i, proxy := range cfg.TrustedProxies {
		cfg.TrustedProxies[i] = strings.TrimSpace(proxy)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// configPath returns the config file path from the --config flag in args or the
// CONFIG_PATH env. A flag error is reported on stderr by the FlagSet itself.
func configPath(args []string) string {
	var path string
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.StringVar(&path, "config", "", "path to YAML config file")
	_ = fs.Parse(args)

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
	defer func() { _ = f.Close() }()

	if err = yaml.NewDecoder(f).Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
