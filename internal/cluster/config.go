package cluster

import (
	"fmt"
	"strings"
)

// Backend picks the storage + replication strategy at startup.
type Backend string

const (
	// BackendStandalone runs as a single-node deployment using a
	// durable disk-backed store. Explicit Standalone.Ephemeral opts into memory. Always leader. Default.
	BackendStandalone Backend = "standalone"

	// BackendRaft runs as a multi-replica HA cluster. Replicas keep their
	// in-memory stores in sync via hashicorp/raft.
	BackendRaft Backend = "raft"
)

// Config is the top-level cluster configuration.
type Config struct {
	Standalone StandaloneConfig `yaml:"standalone" envconfig:"STANDALONE"`
	Backend    Backend          `yaml:"backend"   envconfig:"BACKEND"`
	Raft       RaftConfig       `yaml:"raft"      envconfig:"RAFT"`
	Discovery  DiscoveryConfig  `yaml:"discovery" envconfig:"DISCOVERY"`
}

// DefaultStandaloneDataDir is relative to the process working directory.
const DefaultStandaloneDataDir = "data/podium"

// StandaloneConfig configures persistent single-node storage.
// Ephemeral explicitly opts into volatile development state and ignores DataDir.
type StandaloneConfig struct {
	Ephemeral bool   `yaml:"ephemeral" envconfig:"EPHEMERAL"`
	DataDir   string `yaml:"data_dir" envconfig:"DATA_DIR"`
}

// RaftConfig parametrises the raft backend.
type RaftConfig struct {
	NodeID             string `yaml:"node_id"           envconfig:"NODE_ID"`
	BindAddr           string `yaml:"bind_addr"         envconfig:"BIND_ADDR"`
	AdvertiseAddr      string `yaml:"advertise_addr"    envconfig:"ADVERTISE_ADDR"`
	DataDir            string `yaml:"data_dir"          envconfig:"DATA_DIR"`
	ElectionTimeoutMs  int    `yaml:"election_timeout_ms" envconfig:"ELECTION_TIMEOUT_MS"`
	HeartbeatTimeoutMs int    `yaml:"heartbeat_timeout_ms" envconfig:"HEARTBEAT_TIMEOUT_MS"`
}

// DiscoveryConfig picks a Discovery driver and its parameters.
type DiscoveryConfig struct {
	Driver   string   `yaml:"driver"   envconfig:"DRIVER"`   // "static" or "dns"
	Peers    []string `yaml:"peers"    envconfig:"PEERS"`    // for "static"
	Hostname string   `yaml:"hostname" envconfig:"HOSTNAME"` // for "dns"
	Port     int      `yaml:"port"     envconfig:"PORT"`     // for "dns"
}

// DefaultConfig returns a standalone config.
func DefaultConfig() Config {
	return Config{Backend: BackendStandalone, Standalone: StandaloneConfig{DataDir: DefaultStandaloneDataDir}}
}

// Validate requires a persistent standalone location unless ephemeral mode is
// explicitly selected. Ephemeral takes precedence over a default/yaml/env path.
func (c Config) Validate() error {
	switch c.Backend {
	case "", BackendStandalone:
		if !c.Standalone.Ephemeral && strings.TrimSpace(c.Standalone.DataDir) == "" {
			return fmt.Errorf("standalone.data_dir is required unless standalone.ephemeral is explicitly true")
		}
	case BackendRaft:
	default:
		return fmt.Errorf("unknown backend %q", c.Backend)
	}
	return nil
}

// DataDir returns the data directory of the selected backend: Standalone.DataDir,
// or Raft.DataDir in raft mode. Ephemeral standalone state has none.
func (c Config) DataDir() string {
	switch c.Backend {
	case "", BackendStandalone:
		if c.Standalone.Ephemeral {
			return ""
		}
		return c.Standalone.DataDir
	case BackendRaft:
		return c.Raft.DataDir
	}
	return ""
}
