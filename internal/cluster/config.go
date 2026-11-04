package cluster

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
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

// DefaultStandaloneSnapshotThreshold is the number of log entries after which the
// durable store writes a snapshot and truncates its log.
const DefaultStandaloneSnapshotThreshold = 4096

// StandaloneConfig configures persistent single-node storage.
// Ephemeral explicitly opts into volatile development state and ignores DataDir.
// SnapshotThreshold is the number of log entries that starts a snapshot; zero
// keeps the default.
type StandaloneConfig struct {
	Ephemeral         bool   `yaml:"ephemeral" envconfig:"EPHEMERAL"`
	DataDir           string `yaml:"data_dir" envconfig:"DATA_DIR"`
	SnapshotThreshold int    `yaml:"snapshot_threshold" envconfig:"SNAPSHOT_THRESHOLD"`
}

// RaftConfig parametrises the raft backend; no other backend reads it.
// AdvertiseAddr is the address peers dial, BindAddr when it is empty.
type RaftConfig struct {
	NodeID             string `yaml:"node_id"           envconfig:"NODE_ID"`
	BindAddr           string `yaml:"bind_addr"         envconfig:"BIND_ADDR"`
	AdvertiseAddr      string `yaml:"advertise_addr"    envconfig:"ADVERTISE_ADDR"`
	DataDir            string `yaml:"data_dir"          envconfig:"DATA_DIR"`
	ElectionTimeoutMs  int    `yaml:"election_timeout_ms" envconfig:"ELECTION_TIMEOUT_MS"`
	HeartbeatTimeoutMs int    `yaml:"heartbeat_timeout_ms" envconfig:"HEARTBEAT_TIMEOUT_MS"`
}

// advertise returns the address this node gives its peers.
func (r RaftConfig) advertise() string {
	if r.AdvertiseAddr != "" {
		return r.AdvertiseAddr
	}
	return r.BindAddr
}

// DiscoveryConfig picks the Discovery driver a raft node bootstraps from, and
// its parameters; no other backend reads it.
type DiscoveryConfig struct {
	Driver   string   `yaml:"driver"   envconfig:"DRIVER"`   // "static" or "dns"
	Peers    []string `yaml:"peers"    envconfig:"PEERS"`    // for "static": id=host:port or host:port
	Hostname string   `yaml:"hostname" envconfig:"HOSTNAME"` // for "dns"
	Port     int      `yaml:"port"     envconfig:"PORT"`     // for "dns": every node's raft port
}

// Discovery drivers of [DiscoveryConfig.Driver]. An empty driver is static.
const (
	DriverStatic = "static"
	DriverDNS    = "dns"
)

var (
	// ErrRaftRequired indicates an empty raft.node_id, raft.bind_addr or
	// raft.data_dir with backend raft.
	ErrRaftRequired = errors.New("required when backend is raft")

	// ErrAdvertiseRequired indicates a raft.bind_addr that listens on every
	// interface without a raft.advertise_addr to give peers instead.
	ErrAdvertiseRequired = errors.New("raft.advertise_addr is required")

	// ErrInvalidPeer indicates a discovery.peers entry that is neither
	// id=host:port nor host:port.
	ErrInvalidPeer = errors.New("a peer must be id=host:port or host:port, with a port from 1 to 65535")

	// ErrNodeIDNotAdvertise indicates peers named by their address, bare
	// host:port entries or DNS answers, while raft.node_id is not this node's
	// advertise address.
	ErrNodeIDNotAdvertise = errors.New("raft.node_id must be this node's advertise address")

	// ErrNodeNotListed indicates a peer list that does not list this node as
	// raft.node_id at its advertise address.
	ErrNodeNotListed = errors.New("the peers do not list this node as raft.node_id at its advertise address")

	// ErrDuplicatePeer indicates a peer list that names an ID or an address twice.
	ErrDuplicatePeer = errors.New("a peer ID or address is listed twice")
)

// StaticPeers parses Peers, the list of the static driver. An entry is
// id=host:port, or a bare host:port whose ID is the address itself. Spaces
// around an entry, its ID and its address are trimmed: the environment
// variable is split on commas only.
func (d DiscoveryConfig) StaticPeers() ([]Peer, error) {
	peers, _, err := d.staticPeers()
	return peers, err
}

// staticPeers is StaticPeers that also reports whether any entry is bare.
func (d DiscoveryConfig) staticPeers() ([]Peer, bool, error) {
	var (
		peers = make([]Peer, 0, len(d.Peers))
		bare  bool
	)
	for _, entry := range d.Peers {
		id, addr, named := strings.Cut(entry, "=")
		if !named {
			id, addr = entry, entry
		}
		p := Peer{ID: strings.TrimSpace(id), Address: strings.TrimSpace(addr)}
		if err := checkPeer(p); err != nil {
			return nil, false, fmt.Errorf("discovery.peers entry %q: %w: %w", entry, ErrInvalidPeer, err)
		}
		peers = append(peers, p)
		bare = bare || !named
	}
	return peers, bare, nil
}

// checkPeer says what is wrong with a parsed peer entry, if anything.
func checkPeer(p Peer) error {
	host, err := hostPort(p.Address)
	switch {
	case p.ID == "" && p.Address == "":
		return errors.New("the entry is empty")
	case p.ID == "":
		return errors.New("the ID is empty")
	case strings.Contains(p.Address, "="):
		return errors.New("the entry has more than one =")
	case err != nil:
		return err
	case host == "":
		return errors.New("the address has no host")
	}
	return nil
}

// ServerList returns peers as the server list every node of a new cluster
// bootstraps with, sorted by ID: whatever order discovery reports them in,
// every node derives the same list. It refuses a list that names an ID or an
// address twice, or that does not list this node as nodeID at its advertise
// address, compared as written: the nodes would disagree on their cluster.
func ServerList(peers []Peer, nodeID, advertise string) ([]Peer, error) {
	if len(peers) == 0 {
		return nil, fmt.Errorf("%w: the peer list is empty; list every node, this one included", ErrNodeNotListed)
	}
	var (
		ids    = make(map[string]bool, len(peers))
		addrs  = make(map[string]bool, len(peers))
		listed bool
	)
	for _, p := range peers {
		if ids[p.ID] {
			return nil, fmt.Errorf("%w: ID %q", ErrDuplicatePeer, p.ID)
		}
		if addrs[p.Address] {
			return nil, fmt.Errorf("%w: address %q", ErrDuplicatePeer, p.Address)
		}
		ids[p.ID], addrs[p.Address] = true, true
		switch {
		case p.ID == nodeID && p.Address == advertise:
			listed = true
		case p.ID == nodeID:
			return nil, fmt.Errorf("%w: %q is listed at %q, but this node advertises %q", ErrNodeNotListed, nodeID, p.Address, advertise)
		case p.Address == advertise:
			return nil, fmt.Errorf("%w: this node's advertise address %q is listed as %q, but raft.node_id is %q", ErrNodeNotListed, advertise, p.ID, nodeID)
		}
	}
	if !listed {
		return nil, fmt.Errorf("%w: no peer is %q at %q", ErrNodeNotListed, nodeID, advertise)
	}
	sorted := slices.Clone(peers)
	slices.SortFunc(sorted, func(a, b Peer) int { return strings.Compare(a.ID, b.ID) })
	return sorted, nil
}

// DefaultConfig returns a standalone config.
func DefaultConfig() Config {
	return Config{Backend: BackendStandalone, Standalone: StandaloneConfig{
		DataDir:           DefaultStandaloneDataDir,
		SnapshotThreshold: DefaultStandaloneSnapshotThreshold,
	}}
}

// Validate requires a persistent standalone location unless ephemeral mode is
// explicitly selected. Ephemeral takes precedence over a default/yaml/env path.
// A negative snapshot threshold is refused. Backend raft checks its own
// settings and the peers instead (see validateRaft); standalone reads neither.
func (c Config) Validate() error {
	switch c.Backend {
	case "", BackendStandalone:
		if !c.Ephemeral() && strings.TrimSpace(c.Standalone.DataDir) == "" {
			return fmt.Errorf("standalone.data_dir is required unless standalone.ephemeral is explicitly true")
		}
		if c.Standalone.SnapshotThreshold < 0 {
			return fmt.Errorf("standalone.snapshot_threshold must not be negative")
		}
	case BackendRaft:
		return c.validateRaft()
	default:
		return fmt.Errorf("unknown backend %q", c.Backend)
	}
	return nil
}

// validateRaft fails a raft node before it opens anything: node_id, bind_addr
// and data_dir are required; bind_addr and advertise_addr are host:port, and a
// bind_addr on every interface needs an advertise_addr; the peers must list
// this node as node_id at its advertise address.
func (c Config) validateRaft() error {
	r := c.Raft
	for _, setting := range []struct{ key, value string }{
		{"raft.node_id", r.NodeID},
		{"raft.bind_addr", r.BindAddr},
		{"raft.data_dir", r.DataDir},
	} {
		if strings.TrimSpace(setting.value) == "" {
			return fmt.Errorf("%s: %w", setting.key, ErrRaftRequired)
		}
	}
	bindHost, err := hostPort(r.BindAddr)
	if err != nil {
		return fmt.Errorf("raft.bind_addr %q: %w", r.BindAddr, err)
	}
	if r.AdvertiseAddr == "" && everyInterface(bindHost) {
		return fmt.Errorf("raft.bind_addr %q listens on every interface: %w", r.BindAddr, ErrAdvertiseRequired)
	}
	if r.AdvertiseAddr != "" {
		advertiseHost, err := hostPort(r.AdvertiseAddr)
		if err != nil {
			return fmt.Errorf("raft.advertise_addr %q: %w", r.AdvertiseAddr, err)
		}
		if everyInterface(advertiseHost) {
			return fmt.Errorf("raft.advertise_addr %q names no host for peers to dial", r.AdvertiseAddr)
		}
	}
	return c.Discovery.validate(r.NodeID, r.advertise())
}

// validate checks the peers against this node's ID and advertise address.
// Static peers must pass [ServerList] as configured. DNS answers are known
// only at start, so with dns this node must be named by its ip:port at the
// discovery port, the form DNS lists it in, and ServerList runs at start.
func (d DiscoveryConfig) validate(nodeID, advertise string) error {
	switch d.Driver {
	case "", DriverStatic:
		peers, bare, err := d.staticPeers()
		if err != nil {
			return err
		}
		if bare && nodeID != advertise {
			return fmt.Errorf("%w (%q): bare host:port peers are named by their address, and raft.node_id is %q; name every peer id=host:port to choose IDs", ErrNodeIDNotAdvertise, advertise, nodeID)
		}
		_, err = ServerList(peers, nodeID, advertise)
		return err
	case DriverDNS:
		if strings.TrimSpace(d.Hostname) == "" {
			return errors.New("discovery.hostname is required with discovery.driver dns")
		}
		if d.Port < 1 || d.Port > 65535 {
			return fmt.Errorf("discovery.port %d is not a port from 1 to 65535", d.Port)
		}
		host, _, _ := net.SplitHostPort(advertise)
		if ip := net.ParseIP(host); ip == nil || advertise != net.JoinHostPort(ip.String(), strconv.Itoa(d.Port)) {
			return fmt.Errorf("%w: discovery.driver dns lists every node as ip:%d, written as DNS returns it, and this node advertises %q", ErrNodeNotListed, d.Port, advertise)
		}
		if nodeID != advertise {
			return fmt.Errorf("%w (%q): discovery.driver dns names every node by its ip:port, and raft.node_id is %q", ErrNodeIDNotAdvertise, advertise, nodeID)
		}
		return nil
	default:
		return fmt.Errorf("unknown discovery.driver %q", d.Driver)
	}
}

// hostPort splits addr, which must be host:port with a port from 1 to 65535,
// and returns its host, which may be empty.
func hostPort(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return "", fmt.Errorf("port %q is not a number from 1 to 65535", port)
	}
	return host, nil
}

// everyInterface reports whether host, as a listen host, is every interface:
// empty, 0.0.0.0 or ::.
func everyInterface(host string) bool {
	ip := net.ParseIP(host)
	return host == "" || (ip != nil && ip.IsUnspecified())
}

// DataDir returns the data directory of the selected backend: Standalone.DataDir,
// or Raft.DataDir in raft mode. Ephemeral standalone state has none.
func (c Config) DataDir() string {
	if c.Ephemeral() {
		return ""
	}
	switch c.Backend {
	case "", BackendStandalone:
		return c.Standalone.DataDir
	case BackendRaft:
		return c.Raft.DataDir
	}
	return ""
}

// Ephemeral reports whether the selected backend keeps its state in memory only,
// which only standalone with Standalone.Ephemeral does.
func (c Config) Ephemeral() bool {
	standalone := c.Backend == "" || c.Backend == BackendStandalone
	return standalone && c.Standalone.Ephemeral
}
