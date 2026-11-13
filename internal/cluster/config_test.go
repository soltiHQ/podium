package cluster_test

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config/settings"
	"gopkg.in/yaml.v3"
)

func TestMemoryIsTheDefaultBackend(t *testing.T) {
	cfg := cluster.DefaultConfig()
	if cfg.Backend != cluster.BackendMemory {
		t.Fatalf("default backend = %q, want %q", cfg.Backend, cluster.BackendMemory)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A backend that is neither memory nor raft is refused, and the error names the
// two there are.
func TestUnknownBackendNamesTheBackends(t *testing.T) {
	for _, backend := range []cluster.Backend{"standalone", "other"} {
		cfg := cluster.DefaultConfig()
		cfg.Backend = backend
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), string(backend)) || !strings.Contains(err.Error(), "memory, raft") {
			t.Fatalf("backend %q: Validate() = %v, want an error naming it and memory, raft", backend, err)
		}
	}
}

// The raft log compaction settings load from YAML and from the environment, over
// defaults, and a zero value keeps the default; a negative one is refused. The
// interval is a duration string.
func TestRaftCompactionConfig(t *testing.T) {
	type compaction struct {
		interval            time.Duration
		threshold, trailing int
	}
	for _, tc := range []struct {
		name, yaml string
		env        map[string]string
		want       compaction
		valid      bool
	}{
		{name: "defaults", want: compaction{120 * time.Second, 8192, 10240}, valid: true},
		{name: "yaml", yaml: "raft:\n  snapshot_interval: 30s\n  snapshot_threshold: 512\n  trailing_logs: 100\n", want: compaction{30 * time.Second, 512, 100}, valid: true},
		{name: "env", env: map[string]string{
			"SOLTI_CLUSTER_RAFT_SNAPSHOT_INTERVAL": "2m30s", "SOLTI_CLUSTER_RAFT_SNAPSHOT_THRESHOLD": "1024", "SOLTI_CLUSTER_RAFT_TRAILING_LOGS": "200",
		}, want: compaction{150 * time.Second, 1024, 200}, valid: true},
		{name: "env over yaml", yaml: "raft:\n  trailing_logs: 100\n", env: map[string]string{"SOLTI_CLUSTER_RAFT_TRAILING_LOGS": "300"}, want: compaction{120 * time.Second, 8192, 300}, valid: true},
		{name: "zero keeps the default", yaml: "raft:\n  snapshot_interval: 0s\n  snapshot_threshold: 0\n  trailing_logs: 0\n", want: compaction{0, 0, 0}, valid: true},
		{name: "interval at the minimum", yaml: "raft:\n  snapshot_interval: 5ms\n", want: compaction{5 * time.Millisecond, 8192, 10240}, valid: true},
		{name: "interval under the minimum", yaml: "raft:\n  snapshot_interval: 4ms\n", want: compaction{4 * time.Millisecond, 8192, 10240}},
		{name: "interval of a nanosecond", env: map[string]string{"SOLTI_CLUSTER_RAFT_SNAPSHOT_INTERVAL": "1ns"}, want: compaction{time.Nanosecond, 8192, 10240}},
		{name: "threshold and trailing logs of one", yaml: "raft:\n  snapshot_threshold: 1\n  trailing_logs: 1\n", want: compaction{120 * time.Second, 1, 1}, valid: true},
		{name: "negative interval", yaml: "raft:\n  snapshot_interval: -1s\n", want: compaction{-time.Second, 8192, 10240}},
		{name: "negative threshold", env: map[string]string{"SOLTI_CLUSTER_RAFT_SNAPSHOT_THRESHOLD": "-1"}, want: compaction{120 * time.Second, -1, 10240}},
		{name: "negative trailing logs", yaml: "raft:\n  trailing_logs: -1\n", want: compaction{120 * time.Second, 8192, -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raftConfig()
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			if err := settings.ApplyEnv("SOLTI_CLUSTER", &cfg, os.LookupEnv); err != nil {
				t.Fatal(err)
			}
			if got := (compaction{cfg.Raft.SnapshotInterval, cfg.Raft.SnapshotThreshold, cfg.Raft.TrailingLogs}); got != tc.want {
				t.Fatalf("loaded %+v, want %+v", got, tc.want)
			}
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, want valid %v", err, tc.valid)
			}
		})
	}
}

// Validate refuses the election and heartbeat timeouts that hashicorp/raft would
// refuse when the node starts: each, once 0 keeps its default of 1 s, at 5 ms at
// least, and the election timeout no shorter than the heartbeat timeout, a
// default included. One row per rule, each from YAML or the environment. A
// negative value keeps the default, as 0 does, which the node is given as 0. A
// value over 4611686018427 is refused as too large: hashicorp/raft's heartbeat
// and election timers wait a random time of up to twice the timeout, in a
// time.Duration, which holds no more, and past it the wait wraps negative and
// fires at once. Over 9223372036854 the node's own conversion to a Duration
// wraps already, to a value under 5 ms or to one that passes the other rules.
func TestRaftTimeoutsConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		env        map[string]string
		valid      bool
		says       string // in the error of a value refused
	}{
		{name: "the defaults", valid: true},
		{name: "set", yaml: "raft:\n  election_timeout_ms: 400\n  heartbeat_timeout_ms: 200\n", valid: true},
		{name: "both at the minimum", env: map[string]string{"SOLTI_CLUSTER_RAFT_ELECTION_TIMEOUT_MS": "5", "SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS": "5"}, valid: true},
		{name: "an election timeout equal to the heartbeat timeout", yaml: "raft:\n  election_timeout_ms: 300\n  heartbeat_timeout_ms: 300\n", valid: true},
		{name: "a heartbeat timeout under the default election timeout", yaml: "raft:\n  heartbeat_timeout_ms: 500\n", valid: true},
		{name: "negative values keep the defaults", yaml: "raft:\n  election_timeout_ms: -1\n  heartbeat_timeout_ms: -1\n", valid: true},
		{name: "an election timeout under the minimum", yaml: "raft:\n  election_timeout_ms: 4\n  heartbeat_timeout_ms: 4\n", says: "raft.election_timeout_ms"},
		{name: "a heartbeat timeout under the minimum", env: map[string]string{"SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS": "4"}, says: "raft.heartbeat_timeout_ms"},
		{name: "an election timeout under the heartbeat timeout", yaml: "raft:\n  election_timeout_ms: 100\n  heartbeat_timeout_ms: 200\n", says: "raft.election_timeout_ms"},
		{name: "a heartbeat timeout over the default election timeout", env: map[string]string{"SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS": "2000"}, says: "raft.heartbeat_timeout_ms"},
		{name: "both at the largest value", yaml: "raft:\n  election_timeout_ms: 4611686018427\n  heartbeat_timeout_ms: 4611686018427\n", valid: true},
		{name: "an election timeout over the largest value", yaml: "raft:\n  election_timeout_ms: 4611686018428\n", says: "raft.election_timeout_ms 4611686018428 is too large: the largest a node accepts is 4611686018427, as hashicorp/raft's timers wait up to twice the timeout"},
		{name: "a heartbeat timeout over the largest value", env: map[string]string{"SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS": "4611686018428"}, says: "raft.heartbeat_timeout_ms 4611686018428 is too large: the largest a node accepts is 4611686018427, as hashicorp/raft's timers wait up to twice the timeout"},
		{name: "an election timeout that wraps to an accepted duration", yaml: "raft:\n  election_timeout_ms: 18446744074710\n", says: "raft.election_timeout_ms 18446744074710 is too large"},
		{name: "a heartbeat timeout that wraps to an accepted duration", env: map[string]string{"SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS": "18446744073715"}, says: "raft.heartbeat_timeout_ms 18446744073715 is too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raftConfig()
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			if err := settings.ApplyEnv("SOLTI_CLUSTER", &cfg, os.LookupEnv); err != nil {
				t.Fatal(err)
			}
			err := cfg.Validate()
			if (err == nil) != tc.valid || (err != nil && !strings.Contains(err.Error(), tc.says)) {
				t.Fatalf("Validate() = %v, want valid %v, or an error naming %s", err, tc.valid, tc.says)
			}
		})
	}
}

// The timeouts Validate accepts are the ones hashicorp/raft accepts when the
// node starts, and their defaults are hashicorp/raft's: its ValidateConfig
// (v1.8.0, config.go) wants each at 5 ms at least, and the election timeout no
// shorter than the heartbeat timeout; this pins the minimum and the defaults to
// it. The lease, which New lowers to the heartbeat timeout when it is shorter,
// is then within its limits too.
func TestRaftTimeoutLimitsAreHashicorps(t *testing.T) {
	raft := hraft.DefaultConfig()
	if raft.ElectionTimeout != cluster.DefaultRaftElectionTimeout || raft.HeartbeatTimeout != cluster.DefaultRaftHeartbeatTimeout {
		t.Fatalf("cluster defaults %v and %v; hashicorp/raft defaults %v and %v: they must agree",
			cluster.DefaultRaftElectionTimeout, cluster.DefaultRaftHeartbeatTimeout, raft.ElectionTimeout, raft.HeartbeatTimeout)
	}
	validate := func(election, heartbeat time.Duration) error {
		c := hraft.DefaultConfig()
		c.LocalID = "n1"
		c.ElectionTimeout, c.HeartbeatTimeout = election, heartbeat
		c.LeaderLeaseTimeout = min(c.LeaderLeaseTimeout, heartbeat)
		return hraft.ValidateConfig(c)
	}
	least := cluster.MinRaftTimeout
	for _, tc := range []struct {
		name                string
		election, heartbeat time.Duration
		want                bool // whether hashicorp/raft accepts it
	}{
		{"both at the minimum", least, least, true},
		{"an election timeout a nanosecond under it", least - 1, least - 1, false},
		{"a heartbeat timeout a nanosecond under it", least, least - 1, false},
		{"an election timeout under the heartbeat timeout", 100 * time.Millisecond, 200 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(tc.election, tc.heartbeat); (err == nil) != tc.want {
				t.Fatalf("hashicorp/raft's ValidateConfig = %v, want accepted %v", err, tc.want)
			}
		})
	}
}

// The compaction settings Validate accepts are the ones hashicorp/raft accepts when
// the node starts, so a refused value fails before anything is opened. Its
// ValidateConfig (v1.8.0, config.go) limits only the snapshot interval, to 5ms at
// least, and none of the number of entries; this pins the minimum to it.
func TestRaftCompactionLimitsAreHashicorps(t *testing.T) {
	validate := func(mutate func(*hraft.Config)) error {
		c := hraft.DefaultConfig()
		c.LocalID = "n1"
		mutate(c)
		return hraft.ValidateConfig(c)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*hraft.Config)
		want   bool // whether hashicorp/raft accepts it
	}{
		{"the minimum interval", func(c *hraft.Config) { c.SnapshotInterval = cluster.MinRaftSnapshotInterval }, true},
		{"an interval a nanosecond under it", func(c *hraft.Config) { c.SnapshotInterval = cluster.MinRaftSnapshotInterval - 1 }, false},
		{"the least threshold and trailing logs", func(c *hraft.Config) { c.SnapshotThreshold, c.TrailingLogs = 1, 1 }, true},
		{"the greatest threshold and trailing logs", func(c *hraft.Config) { c.SnapshotThreshold, c.TrailingLogs = math.MaxInt64, math.MaxInt64 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(tc.mutate); (err == nil) != tc.want {
				t.Fatalf("hashicorp/raft's ValidateConfig = %v, want accepted %v", err, tc.want)
			}
		})
	}
}

// The defaults of the raft compaction settings are hashicorp/raft's, which a zero
// value of a setting keeps.
func TestRaftCompactionDefaultsAreHashicorps(t *testing.T) {
	raft, defaults := hraft.DefaultConfig(), cluster.DefaultConfig().Raft
	if defaults.SnapshotInterval != raft.SnapshotInterval || uint64(defaults.SnapshotThreshold) != raft.SnapshotThreshold || uint64(defaults.TrailingLogs) != raft.TrailingLogs {
		t.Fatalf("cluster defaults %v, %d and %d; hashicorp/raft defaults %v, %d and %d: they must agree",
			defaults.SnapshotInterval, defaults.SnapshotThreshold, defaults.TrailingLogs, raft.SnapshotInterval, raft.SnapshotThreshold, raft.TrailingLogs)
	}
}

func TestDataDirIsThatOfTheSelectedBackend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   string
	}{
		{"memory has no directory", func(*cluster.Config) {}, ""},
		{"unset backend is memory", func(c *cluster.Config) { c.Backend = "" }, ""},
		{"raft", func(c *cluster.Config) { c.Backend = cluster.BackendRaft }, "/var/lib/solti/raft"},
		{"unknown backend", func(c *cluster.Config) { c.Backend = "other" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cluster.DefaultConfig()
			cfg.Raft.DataDir = "/var/lib/solti/raft"
			tc.mutate(&cfg)
			if got := cfg.DataDir(); got != tc.want {
				t.Fatalf("DataDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEphemeralIsTheMemoryBackend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   bool
	}{
		{"memory", func(*cluster.Config) {}, true},
		{"unset backend", func(c *cluster.Config) { c.Backend = "" }, true},
		{"raft", func(c *cluster.Config) { c.Backend = cluster.BackendRaft }, false},
		{"unknown backend", func(c *cluster.Config) { c.Backend = "other" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cluster.DefaultConfig()
			tc.mutate(&cfg)
			if got := cfg.Ephemeral(); got != tc.want {
				t.Fatalf("Ephemeral() = %v, want %v", got, tc.want)
			}
		})
	}
}

// errAny marks a case that must fail validation with an error that has no
// sentinel of its own.
var errAny = errors.New("any error")

// checkValidate fails the test unless err is what want asks for: nil, any
// error for errAny, or else an error that wraps want.
func checkValidate(t *testing.T, err, want error) {
	t.Helper()
	switch {
	case want == nil && err != nil:
		t.Fatalf("Validate() = %v, want nil", err)
	case want != nil && err == nil:
		t.Fatalf("Validate() = nil, want %v", want)
	case want != nil && want != errAny && !errors.Is(err, want):
		t.Fatalf("Validate() = %v, want %v", err, want)
	}
}

// raftConfig is a valid raft configuration: node n1 binds and advertises
// 10.0.0.1:7000, its transport runs mutual TLS, its static peers name all three
// nodes, and its compaction settings are the defaults.
func raftConfig() cluster.Config {
	cfg := cluster.DefaultConfig()
	cfg.Backend = cluster.BackendRaft
	cfg.Raft.NodeID, cfg.Raft.BindAddr, cfg.Raft.DataDir = "n1", "10.0.0.1:7000", "/var/lib/solti/raft"
	cfg.Raft.TLS = cluster.RaftTLSConfig{
		CertFile: "/etc/solti/raft/n1.crt",
		KeyFile:  "/etc/solti/raft/n1.key",
		CAFile:   "/etc/solti/raft/ca.crt",
	}
	cfg.Discovery.Peers = []string{"n1=10.0.0.1:7000", "n2=10.0.0.2:7000", "n3=10.0.0.3:7000"}
	return cfg
}

// A raft node needs its transport secured: mutual TLS with all three files of
// raft.tls, or raft.allow_insecure_transport for plain TCP. Some of the files
// are no TLS, and the files with the flag leave the transport unclear. Paths
// are only checked to be set; New loads them.
func TestRaftRequiresTLSOrExplicitInsecure(t *testing.T) {
	files := raftConfig().Raft.TLS
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.RaftConfig)
		want   error
	}{
		{"mutual TLS", func(*cluster.RaftConfig) {}, nil},
		{"plain TCP allowed", func(r *cluster.RaftConfig) {
			r.TLS, r.AllowInsecureTransport = cluster.RaftTLSConfig{}, true
		}, nil},
		{"neither", func(r *cluster.RaftConfig) { r.TLS = cluster.RaftTLSConfig{} }, cluster.ErrRaftTLSRequired},
		{"no cert_file", func(r *cluster.RaftConfig) { r.TLS.CertFile = "" }, cluster.ErrRaftTLSRequired},
		{"no key_file", func(r *cluster.RaftConfig) { r.TLS.KeyFile = "" }, cluster.ErrRaftTLSRequired},
		{"no ca_file", func(r *cluster.RaftConfig) { r.TLS.CAFile = "" }, cluster.ErrRaftTLSRequired},
		{"a blank ca_file", func(r *cluster.RaftConfig) { r.TLS.CAFile = " " }, cluster.ErrRaftTLSRequired},
		{"only a ca_file", func(r *cluster.RaftConfig) { r.TLS = cluster.RaftTLSConfig{CAFile: files.CAFile} }, cluster.ErrRaftTLSRequired},
		{"some files with the flag", func(r *cluster.RaftConfig) {
			r.TLS, r.AllowInsecureTransport = cluster.RaftTLSConfig{CertFile: files.CertFile}, true
		}, cluster.ErrRaftInsecureWithTLS},
		{"mutual TLS with the flag", func(r *cluster.RaftConfig) { r.AllowInsecureTransport = true }, cluster.ErrRaftInsecureWithTLS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raftConfig()
			tc.mutate(&cfg.Raft)
			checkValidate(t, cfg.Validate(), tc.want)
		})
	}
}

// A peer entry is id=host:port, or a bare host:port named by its address.
// Spaces around the entry, its ID and its address are trimmed. A refused entry
// is named in the error with what is wrong with it.
func TestPeerEntries(t *testing.T) {
	for _, tc := range []struct {
		entry  string
		want   cluster.Peer
		reason string // in the error of a refused entry
	}{
		{entry: "n1=10.0.0.1:7000", want: cluster.Peer{ID: "n1", Address: "10.0.0.1:7000"}},
		{entry: "10.0.0.1:7000", want: cluster.Peer{ID: "10.0.0.1:7000", Address: "10.0.0.1:7000"}},
		{entry: "[fd00::1]:7000", want: cluster.Peer{ID: "[fd00::1]:7000", Address: "[fd00::1]:7000"}},
		{entry: "podium-0=podium-0.podium:7000", want: cluster.Peer{ID: "podium-0", Address: "podium-0.podium:7000"}},
		{entry: "n1=[fd00::1]:7000", want: cluster.Peer{ID: "n1", Address: "[fd00::1]:7000"}},
		{entry: " n1 = 10.0.0.1:7000 ", want: cluster.Peer{ID: "n1", Address: "10.0.0.1:7000"}},
		{entry: "", reason: "the entry is empty"},
		{entry: "n1=", reason: "missing port"},
		{entry: "=10.0.0.1:7000", reason: "the ID is empty"},
		{entry: "n1=10.0.0.1", reason: "missing port"},
		{entry: "n1=:7000", reason: "the address has no host"},
		{entry: "n1=10.0.0.1:0", reason: `port "0" is not a number from 1 to 65535`},
		{entry: "n1=10.0.0.1:65536", reason: `port "65536" is not a number from 1 to 65535`},
		{entry: "n1=10.0.0.1:raft", reason: `port "raft" is not a number from 1 to 65535`},
		{entry: "n1=n2=10.0.0.1:7000", reason: "the entry has more than one ="},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			peers, err := cluster.DiscoveryConfig{Peers: []string{tc.entry}}.StaticPeers()
			if tc.reason != "" {
				if !errors.Is(err, cluster.ErrInvalidPeer) || !strings.Contains(err.Error(), tc.reason) || !strings.Contains(err.Error(), strconv.Quote(tc.entry)) {
					t.Fatalf("StaticPeers() = %v, %v; want %v naming %q and %q", peers, err, cluster.ErrInvalidPeer, tc.entry, tc.reason)
				}
				return
			}
			if err != nil || len(peers) != 1 || peers[0] != tc.want {
				t.Fatalf("StaticPeers() = %v, %v; want [%v]", peers, err, tc.want)
			}
		})
	}
}

// A bare host:port peer is named by its address. So once any peer is bare,
// raft.node_id must be this node's advertise address: advertise_addr, or
// bind_addr without it. Named id=host:port peers leave the ID free.
func TestBareAddressPeersRequireMatchingNodeID(t *testing.T) {
	bare := []string{"10.0.0.1:7000", "10.0.0.2:7000", "10.0.0.3:7000"}
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   error
	}{
		{"named peers, named node", func(*cluster.Config) {}, nil},
		{"bare peers, node named by its bind address", func(c *cluster.Config) {
			c.Raft.NodeID, c.Discovery.Peers = "10.0.0.1:7000", bare
		}, nil},
		{"bare peers, node named by its advertise address", func(c *cluster.Config) {
			c.Raft.NodeID, c.Raft.BindAddr, c.Raft.AdvertiseAddr = "10.0.0.1:7000", "0.0.0.0:7000", "10.0.0.1:7000"
			c.Discovery.Peers = bare
		}, nil},
		{"bare peers, named node", func(c *cluster.Config) { c.Discovery.Peers = bare }, cluster.ErrNodeIDNotAdvertise},
		{"bare peers, node named by a bind address it does not advertise", func(c *cluster.Config) {
			c.Raft.NodeID, c.Raft.BindAddr, c.Raft.AdvertiseAddr = "192.168.0.1:7000", "192.168.0.1:7000", "10.0.0.1:7000"
			c.Discovery.Peers = bare
		}, cluster.ErrNodeIDNotAdvertise},
		{"named node among bare peers", func(c *cluster.Config) {
			c.Discovery.Peers = []string{"n1=10.0.0.1:7000", "10.0.0.2:7000", "10.0.0.3:7000"}
		}, cluster.ErrNodeIDNotAdvertise},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raftConfig()
			tc.mutate(&cfg)
			checkValidate(t, cfg.Validate(), tc.want)
		})
	}
}

// DNS discovery lists every node as ip:port, named by that address, with the
// port of discovery.port. So this node's advertise address must be its IP and
// that port, written as DNS returns it, and raft.node_id must be the same.
func TestDNSPeersRequireMatchingNodeID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   error
	}{
		{"node named by its ip:port", func(*cluster.Config) {}, nil},
		{"node named by its advertise address", func(c *cluster.Config) {
			c.Raft.BindAddr, c.Raft.AdvertiseAddr = "0.0.0.0:7000", "10.0.0.1:7000"
		}, nil},
		{"IPv6 node", func(c *cluster.Config) {
			c.Raft.NodeID, c.Raft.BindAddr = "[fd00::1]:7000", "[fd00::1]:7000"
		}, nil},
		{"named node", func(c *cluster.Config) { c.Raft.NodeID = "n1" }, cluster.ErrNodeIDNotAdvertise},
		{"advertised by host name", func(c *cluster.Config) {
			c.Raft.NodeID, c.Raft.BindAddr = "podium-0.podium:7000", "podium-0.podium:7000"
		}, cluster.ErrNodeNotListed},
		{"advertised on another port", func(c *cluster.Config) {
			c.Raft.NodeID, c.Raft.BindAddr = "10.0.0.1:7001", "10.0.0.1:7001"
		}, cluster.ErrNodeNotListed},
		{"IPv6 not written as DNS returns it", func(c *cluster.Config) {
			c.Raft.NodeID, c.Raft.BindAddr = "[fd00:0::1]:7000", "[fd00:0::1]:7000"
		}, cluster.ErrNodeNotListed},
		{"no host name", func(c *cluster.Config) { c.Discovery.Hostname = "" }, errAny},
		{"no port", func(c *cluster.Config) { c.Discovery.Port = 0 }, errAny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raftConfig()
			cfg.Raft.NodeID = "10.0.0.1:7000"
			cfg.Discovery = cluster.DiscoveryConfig{Driver: cluster.DriverDNS, Hostname: "podium.svc", Port: 7000}
			tc.mutate(&cfg)
			checkValidate(t, cfg.Validate(), tc.want)
		})
	}
}

// bind_addr and advertise_addr are host:port with a port from 1 to 65535. A
// bind_addr on every interface (0.0.0.0, :: or no host) gives peers nothing to
// dial, so it needs an advertise_addr, which must name one host.
func TestRaftBindAndAdvertiseAddresses(t *testing.T) {
	for _, tc := range []struct {
		name, bind, advertise string
		want                  error
	}{
		{"bind address only", "10.0.0.1:7000", "", nil},
		{"every interface with advertise_addr", "0.0.0.0:7000", "10.0.0.1:7000", nil},
		{"0.0.0.0", "0.0.0.0:7000", "", cluster.ErrAdvertiseRequired},
		{"::", "[::]:7000", "", cluster.ErrAdvertiseRequired},
		{"no host", ":7000", "", cluster.ErrAdvertiseRequired},
		{"advertise_addr on every interface", "0.0.0.0:7000", "0.0.0.0:7000", errAny},
		{"bind address without a port", "10.0.0.1", "", errAny},
		{"bind port 0", "10.0.0.1:0", "", errAny},
		{"bind port out of range", "10.0.0.1:65536", "", errAny},
		{"advertise_addr without a port", "0.0.0.0:7000", "10.0.0.1", errAny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raftConfig()
			cfg.Raft.BindAddr, cfg.Raft.AdvertiseAddr = tc.bind, tc.advertise
			checkValidate(t, cfg.Validate(), tc.want)
		})
	}
}

// The static peers name every ID and every address once and list this node as
// raft.node_id at its advertise address; otherwise the nodes would bootstrap
// different configurations.
func TestRaftPeersListThisNodeOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		peers []string
		want  error
	}{
		{"every node once", []string{"n3=10.0.0.3:7000", "n1=10.0.0.1:7000", "n2=10.0.0.2:7000"}, nil},
		{"only this node", []string{"n1=10.0.0.1:7000"}, nil},
		{"no peers", nil, cluster.ErrNodeNotListed},
		{"this node missing", []string{"n2=10.0.0.2:7000", "n3=10.0.0.3:7000"}, cluster.ErrNodeNotListed},
		{"this node at another address", []string{"n1=10.0.0.9:7000", "n2=10.0.0.2:7000"}, cluster.ErrNodeNotListed},
		{"its address under another ID", []string{"n9=10.0.0.1:7000", "n1=10.0.0.9:7000"}, cluster.ErrNodeNotListed},
		{"an ID twice", []string{"n1=10.0.0.1:7000", "n2=10.0.0.2:7000", "n2=10.0.0.3:7000"}, cluster.ErrDuplicatePeer},
		{"an address twice", []string{"n1=10.0.0.1:7000", "n2=10.0.0.2:7000", "n3=10.0.0.2:7000"}, cluster.ErrDuplicatePeer},
		{"a malformed entry", []string{"n1=10.0.0.1:7000", "n2"}, cluster.ErrInvalidPeer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raftConfig()
			cfg.Discovery.Peers = tc.peers
			checkValidate(t, cfg.Validate(), tc.want)
		})
	}
}

// cluster.raft.join, env SOLTI_CLUSTER_RAFT_JOIN, starts a node that bootstraps
// nothing: the leader adds it. Its peers are never asked, so they need not list
// it and may be empty, while a list that is set must still parse and the driver
// must be one there is. Every other raft setting is checked as for any node, and
// a node that does not join still needs its peers to list it.
func TestJoiningNodeValidation(t *testing.T) {
	t.Run("loads", func(t *testing.T) {
		cfg := cluster.DefaultConfig()
		if err := yaml.Unmarshal([]byte("raft:\n  join: true\n"), &cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.Raft.Join {
			t.Fatal("YAML raft.join not loaded")
		}
		cfg = cluster.DefaultConfig()
		t.Setenv("SOLTI_CLUSTER_RAFT_JOIN", "true")
		if err := settings.ApplyEnv("SOLTI_CLUSTER", &cfg, os.LookupEnv); err != nil {
			t.Fatal(err)
		}
		if !cfg.Raft.Join {
			t.Fatal("SOLTI_CLUSTER_RAFT_JOIN not loaded")
		}
	})
	for _, tc := range []struct {
		name   string
		join   bool
		mutate func(*cluster.Config)
		want   error
	}{
		{"joining, not listed", true, func(c *cluster.Config) {
			c.Discovery.Peers = []string{"n2=10.0.0.2:7000", "n3=10.0.0.3:7000"}
		}, nil},
		{"joining, no peers", true, func(c *cluster.Config) { c.Discovery.Peers = nil }, nil},
		{"joining, listed", true, func(*cluster.Config) {}, nil},
		{"joining, dns", true, func(c *cluster.Config) {
			c.Discovery = cluster.DiscoveryConfig{Driver: cluster.DriverDNS, Hostname: "podium.svc", Port: 7000}
		}, nil},
		{"joining, a malformed entry", true, func(c *cluster.Config) {
			c.Discovery.Peers = []string{"n2=10.0.0.2"}
		}, cluster.ErrInvalidPeer},
		{"joining, an unknown driver", true, func(c *cluster.Config) { c.Discovery.Driver = "consul" }, errAny},
		{"joining, dns without a host name", true, func(c *cluster.Config) {
			c.Discovery = cluster.DiscoveryConfig{Driver: cluster.DriverDNS, Port: 7000}
		}, errAny},
		{"joining, no data_dir", true, func(c *cluster.Config) { c.Raft.DataDir = "" }, cluster.ErrRaftRequired},
		{"joining, no transport security", true, func(c *cluster.Config) { c.Raft.TLS = cluster.RaftTLSConfig{} }, cluster.ErrRaftTLSRequired},
		{"joining, every interface without advertise_addr", true, func(c *cluster.Config) {
			c.Raft.BindAddr = "0.0.0.0:7000"
		}, cluster.ErrAdvertiseRequired},
		{"not joining, not listed", false, func(c *cluster.Config) {
			c.Discovery.Peers = []string{"n2=10.0.0.2:7000", "n3=10.0.0.3:7000"}
		}, cluster.ErrNodeNotListed},
		{"not joining, no peers", false, func(c *cluster.Config) { c.Discovery.Peers = nil }, cluster.ErrNodeNotListed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raftConfig()
			cfg.Raft.Join = tc.join
			tc.mutate(&cfg)
			checkValidate(t, cfg.Validate(), tc.want)
		})
	}
}

// Memory reads no raft or discovery setting, so invalid ones do not fail its
// validation.
func TestMemoryIgnoresRaftConfig(t *testing.T) {
	for _, backend := range []cluster.Backend{"", cluster.BackendMemory} {
		cfg := cluster.DefaultConfig()
		cfg.Backend = backend
		cfg.Raft = cluster.RaftConfig{BindAddr: "0.0.0.0:7000", TLS: cluster.RaftTLSConfig{CertFile: "n1.crt"}, AllowInsecureTransport: true, SnapshotInterval: -1, SnapshotThreshold: -1, TrailingLogs: -1}
		cfg.Discovery = cluster.DiscoveryConfig{Driver: "consul", Peers: []string{"=10.0.0.1:7000"}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("backend %q: Validate() = %v, want nil", backend, err)
		}
	}
}
