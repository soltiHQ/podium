package cluster

// Backend picks the storage + replication strategy at startup.
type Backend string

const (
	// BackendStandalone runs as a single-node deployment using an
	// in-memory store. Always leader. Default.
	BackendStandalone Backend = "standalone"

	// BackendRaft runs as a multi-replica HA cluster. Replicas keep their
	// in-memory stores in sync via hashicorp/raft.
	BackendRaft Backend = "raft"
)

// Config is the top-level cluster configuration.
type Config struct {
	Backend   Backend         `yaml:"backend"   envconfig:"BACKEND"`
	Raft      RaftConfig      `yaml:"raft"      envconfig:"RAFT"`
	Discovery DiscoveryConfig `yaml:"discovery" envconfig:"DISCOVERY"`
}

// RaftConfig parametrises the raft backend.
type RaftConfig struct {
	NodeID           string `yaml:"node_id"           envconfig:"NODE_ID"`
	BindAddr         string `yaml:"bind_addr"         envconfig:"BIND_ADDR"`
	AdvertiseAddr    string `yaml:"advertise_addr"    envconfig:"ADVERTISE_ADDR"`
	DataDir          string `yaml:"data_dir"          envconfig:"DATA_DIR"`
	ElectionTimeoutMs int    `yaml:"election_timeout_ms" envconfig:"ELECTION_TIMEOUT_MS"`
	HeartbeatTimeoutMs int   `yaml:"heartbeat_timeout_ms" envconfig:"HEARTBEAT_TIMEOUT_MS"`
}

// DiscoveryConfig picks a Discovery driver and its parameters.
type DiscoveryConfig struct {
	Driver   string   `yaml:"driver"   envconfig:"DRIVER"` // "static" or "dns"
	Peers    []string `yaml:"peers"    envconfig:"PEERS"`   // for "static"
	Hostname string   `yaml:"hostname" envconfig:"HOSTNAME"` // for "dns"
	Port     int      `yaml:"port"     envconfig:"PORT"`     // for "dns"
}

// DefaultConfig returns a standalone config.
func DefaultConfig() Config {
	return Config{Backend: BackendStandalone}
}
