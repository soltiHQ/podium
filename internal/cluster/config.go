package cluster

import (
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
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
	Standalone StandaloneConfig `yaml:"standalone"`
	Backend    Backend          `yaml:"backend"`
	Raft       RaftConfig       `yaml:"raft"`
	Discovery  DiscoveryConfig  `yaml:"discovery"`
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
	Ephemeral         bool   `yaml:"ephemeral"`
	DataDir           string `yaml:"data_dir"`
	SnapshotThreshold int    `yaml:"snapshot_threshold"`
}

// The defaults of the raft log compaction settings of [RaftConfig]: they are
// hashicorp/raft's own, which a zero value of a setting keeps.
const (
	// DefaultRaftSnapshotInterval is how often a node checks whether to snapshot.
	DefaultRaftSnapshotInterval = 120 * time.Second
	// DefaultRaftSnapshotThreshold is the number of log entries since the last
	// snapshot that starts the next one.
	DefaultRaftSnapshotThreshold = 8192
	// DefaultRaftTrailingLogs is the number of log entries a snapshot leaves in
	// the log, for a slow follower to catch up from.
	DefaultRaftTrailingLogs = 10240

	// MinRaftSnapshotInterval is the shortest snapshot interval hashicorp/raft
	// accepts: its ValidateConfig refuses a shorter one when a node starts
	// (v1.8.0, config.go: SnapshotInterval < 5ms). It has no other limit on the
	// compaction settings.
	MinRaftSnapshotInterval = 5 * time.Millisecond
)

// The raft timeouts of [RaftConfig]: hashicorp/raft's defaults, which an
// ElectionTimeoutMs or HeartbeatTimeoutMs of 0 or less keeps, and its limits.
const (
	// DefaultRaftElectionTimeout is the election timeout a node keeps without
	// raft.election_timeout_ms.
	DefaultRaftElectionTimeout = time.Second
	// DefaultRaftHeartbeatTimeout is the heartbeat timeout a node keeps without
	// raft.heartbeat_timeout_ms.
	DefaultRaftHeartbeatTimeout = time.Second
	// MinRaftTimeout is the shortest election or heartbeat timeout
	// hashicorp/raft accepts: its ValidateConfig refuses a shorter one when a
	// node starts (v1.8.0, config.go), and an election timeout shorter than the
	// heartbeat timeout too.
	MinRaftTimeout = 5 * time.Millisecond
)

// RaftConfig parametrises the raft backend; no other backend reads it.
// AdvertiseAddr is the address peers dial, BindAddr when it is empty. The raft
// transport runs mutual TLS with the files of TLS, or plain TCP when
// AllowInsecureTransport is true; it needs one of the two.
//
// SnapshotInterval, SnapshotThreshold and TrailingLogs set how the node
// compacts its log: it checks every SnapshotInterval whether SnapshotThreshold
// entries have piled up since its last snapshot, and a snapshot leaves
// TrailingLogs entries in the log. A zero value keeps the default; a negative
// one is refused, and so is a SnapshotInterval under MinRaftSnapshotInterval.
//
// Join starts a node that holds no raft state yet without bootstrapping a
// cluster: it waits for the leader of a running cluster to add it, through the
// membership API. Discovery is then not asked, so the peers need not list the
// node and may be empty. A node with raft state rejoins its cluster from its
// own log whatever Join says.
type RaftConfig struct {
	NodeID             string `yaml:"node_id"`
	BindAddr           string `yaml:"bind_addr"`
	AdvertiseAddr      string `yaml:"advertise_addr"`
	DataDir            string `yaml:"data_dir"`
	Join               bool   `yaml:"join"`
	ElectionTimeoutMs  int    `yaml:"election_timeout_ms"`
	HeartbeatTimeoutMs int    `yaml:"heartbeat_timeout_ms"`

	SnapshotInterval  time.Duration `yaml:"snapshot_interval"`
	SnapshotThreshold int           `yaml:"snapshot_threshold"`
	TrailingLogs      int           `yaml:"trailing_logs"`

	TLS                    RaftTLSConfig `yaml:"tls"`
	AllowInsecureTransport bool          `yaml:"allow_insecure_transport"`
}

// RaftTLSConfig names the PEM files of mutual TLS between raft nodes. A node
// presents one certificate, CertFile with its KeyFile, both to the peers that
// connect to it and to the peers it dials, and accepts a peer only with a
// certificate that CAFile verifies, over TLS 1.3. A peer it dials must also
// present a certificate for the host it dials: the host of the peer's address
// as the peers list it, as an IP or DNS subject alternative name.
type RaftTLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`
}

// Enabled reports whether any file of t is set, which asks for mutual TLS.
func (t RaftTLSConfig) Enabled() bool {
	return strings.TrimSpace(t.CertFile) != "" || strings.TrimSpace(t.KeyFile) != "" || strings.TrimSpace(t.CAFile) != ""
}

// validate requires all three files, or none with allowInsecure, the explicit
// choice of plain TCP. Only some of the files are no TLS, and files with
// allowInsecure leave unclear which transport is meant. The node reads the
// files when it starts.
func (t RaftTLSConfig) validate(allowInsecure bool) error {
	var missing []string
	for _, file := range []struct{ key, value string }{
		{"raft.tls.cert_file", t.CertFile},
		{"raft.tls.key_file", t.KeyFile},
		{"raft.tls.ca_file", t.CAFile},
	} {
		if strings.TrimSpace(file.value) == "" {
			missing = append(missing, file.key)
		}
	}
	switch {
	case !t.Enabled() && allowInsecure:
		return nil
	case allowInsecure:
		return ErrRaftInsecureWithTLS
	case !t.Enabled():
		return ErrRaftTLSRequired
	case len(missing) > 0:
		return fmt.Errorf("%s not set: %w", strings.Join(missing, ", "), ErrRaftTLSRequired)
	}
	return nil
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
	Driver   string   `yaml:"driver"`   // "static" or "dns"
	Peers    []string `yaml:"peers"`    // for "static": id=host:port or host:port
	Hostname string   `yaml:"hostname"` // for "dns"
	Port     int      `yaml:"port"`     // for "dns": every node's raft port
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

	// ErrRaftTLSRequired indicates a raft node with neither all three files of
	// raft.tls nor raft.allow_insecure_transport.
	ErrRaftTLSRequired = errors.New("the raft transport needs raft.tls.cert_file, raft.tls.key_file and raft.tls.ca_file for mutual TLS, or raft.allow_insecure_transport: true for plain TCP")

	// ErrRaftInsecureWithTLS indicates raft.allow_insecure_transport set with a
	// file of raft.tls: which transport is meant is unclear.
	ErrRaftInsecureWithTLS = errors.New("raft.allow_insecure_transport is true while raft.tls is set: set either, for plain TCP or for mutual TLS")

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

// DefaultConfig returns a standalone config. Its raft settings are the
// defaults of the compaction settings; the other raft settings have none.
func DefaultConfig() Config {
	return Config{
		Backend: BackendStandalone,
		Standalone: StandaloneConfig{
			DataDir:           DefaultStandaloneDataDir,
			SnapshotThreshold: DefaultStandaloneSnapshotThreshold,
		},
		Raft: RaftConfig{
			SnapshotInterval:  DefaultRaftSnapshotInterval,
			SnapshotThreshold: DefaultRaftSnapshotThreshold,
			TrailingLogs:      DefaultRaftTrailingLogs,
		},
	}
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
// and data_dir are required, and so is the transport's mutual TLS or the
// explicit choice of plain TCP; the compaction settings are not negative, and the
// snapshot interval and the timeouts are ones hashicorp/raft accepts (see
// validateTimeouts); bind_addr and advertise_addr are host:port, and a bind_addr
// on every interface needs an advertise_addr; the peers must list this node as
// node_id at its advertise address, unless it joins a running cluster (see
// validateJoining).
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
	if err := r.TLS.validate(r.AllowInsecureTransport); err != nil {
		return err
	}
	for _, setting := range []struct {
		key      string
		negative bool
	}{
		{"raft.snapshot_interval", r.SnapshotInterval < 0},
		{"raft.snapshot_threshold", r.SnapshotThreshold < 0},
		{"raft.trailing_logs", r.TrailingLogs < 0},
	} {
		if setting.negative {
			return fmt.Errorf("%s must not be negative", setting.key)
		}
	}
	if r.SnapshotInterval > 0 && r.SnapshotInterval < MinRaftSnapshotInterval {
		return fmt.Errorf("raft.snapshot_interval %s is under the %s that hashicorp/raft accepts", r.SnapshotInterval, MinRaftSnapshotInterval)
	}
	if err := r.validateTimeouts(); err != nil {
		return err
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
	if r.Join {
		return c.Discovery.validateJoining()
	}
	return c.Discovery.validate(r.NodeID, r.advertise())
}

// maxRaftTimeoutMs is the largest election or heartbeat timeout, in
// milliseconds, that a node accepts: the most whole milliseconds a
// time.Duration holds, as the node runs with the timeout as one.
const maxRaftTimeoutMs = math.MaxInt64 / int64(time.Millisecond)

// validateTimeouts refuses the election and heartbeat timeouts that
// hashicorp/raft refuses when the node starts, in BootstrapCluster or NewRaft,
// once the stores are open and the transport listens: each, a value of 0 or
// less taken as its default, must be MinRaftTimeout at least, and the election
// timeout no shorter than the heartbeat timeout. The lease, which the node
// lowers to the heartbeat timeout when that is shorter, is then within
// hashicorp/raft's limits as well, and nothing else of its ValidateConfig
// depends on the configuration but node_id and the snapshot interval. A value
// over maxRaftTimeoutMs is refused first, as too large: as a time.Duration it
// would wrap, to a value that the other rules would misjudge.
func (r RaftConfig) validateTimeouts() error {
	election := raftTimeout(r.ElectionTimeoutMs, DefaultRaftElectionTimeout)
	heartbeat := raftTimeout(r.HeartbeatTimeoutMs, DefaultRaftHeartbeatTimeout)
	switch {
	case int64(r.ElectionTimeoutMs) > maxRaftTimeoutMs:
		return fmt.Errorf("raft.election_timeout_ms %d is too large: the largest a node accepts is %d", r.ElectionTimeoutMs, maxRaftTimeoutMs)
	case int64(r.HeartbeatTimeoutMs) > maxRaftTimeoutMs:
		return fmt.Errorf("raft.heartbeat_timeout_ms %d is too large: the largest a node accepts is %d", r.HeartbeatTimeoutMs, maxRaftTimeoutMs)
	case election < MinRaftTimeout:
		return fmt.Errorf("raft.election_timeout_ms %d is under the %s that hashicorp/raft accepts", r.ElectionTimeoutMs, MinRaftTimeout)
	case heartbeat < MinRaftTimeout:
		return fmt.Errorf("raft.heartbeat_timeout_ms %d is under the %s that hashicorp/raft accepts", r.HeartbeatTimeoutMs, MinRaftTimeout)
	case election < heartbeat:
		return fmt.Errorf("raft.election_timeout_ms (%s) is under raft.heartbeat_timeout_ms (%s): hashicorp/raft needs the election timeout to be at least the heartbeat timeout; 0 keeps the default of %s", election, heartbeat, DefaultRaftElectionTimeout)
	}
	return nil
}

// raftTimeout is the timeout of ms milliseconds that a node runs with, or def
// for 0 or less, as the node gives hashicorp/raft no value then. It wraps for
// ms over maxRaftTimeoutMs, which validateTimeouts refuses.
func raftTimeout(ms int, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

// validateJoining checks the discovery of a node that joins a running cluster
// (RaftConfig.Join). The node bootstraps nothing and never asks discovery for
// its peers, so the static list need not list it and may be empty, and DNS
// answers need not name it. What is set must still be usable, as the node builds
// its discovery all the same: a static list that parses, a known driver, and the
// host name and port of dns.
func (d DiscoveryConfig) validateJoining() error {
	switch d.Driver {
	case "", DriverStatic:
		_, _, err := d.staticPeers()
		return err
	case DriverDNS:
		return d.checkDNS()
	default:
		return fmt.Errorf("unknown discovery.driver %q", d.Driver)
	}
}

// checkDNS requires the host name and the port that the dns driver resolves.
func (d DiscoveryConfig) checkDNS() error {
	if strings.TrimSpace(d.Hostname) == "" {
		return errors.New("discovery.hostname is required with discovery.driver dns")
	}
	if d.Port < 1 || d.Port > 65535 {
		return fmt.Errorf("discovery.port %d is not a port from 1 to 65535", d.Port)
	}
	return nil
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
		if err := d.checkDNS(); err != nil {
			return err
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

// CheckDialAddress says what is wrong with addr as an address that peers dial,
// if anything: it must be host:port with a port from 1 to 65535, and name one
// host, as raft.advertise_addr must, not none or every interface.
func CheckDialAddress(addr string) error {
	host, err := hostPort(addr)
	if err != nil {
		return err
	}
	if everyInterface(host) {
		return fmt.Errorf("%q names no host for peers to dial", addr)
	}
	return nil
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
