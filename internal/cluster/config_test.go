package cluster_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/kelseyhightower/envconfig"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage/durable"
	"gopkg.in/yaml.v3"
)

func TestStandaloneDataDirectoryConfig(t *testing.T) {
	cfg := cluster.DefaultConfig()
	if cfg.Backend != cluster.BackendStandalone || cfg.Standalone.DataDir != cluster.DefaultStandaloneDataDir {
		t.Fatal("durable default missing")
	}
	if err := yaml.Unmarshal([]byte("standalone:\n  data_dir: /var/lib/solti/podium\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Standalone.DataDir != "/var/lib/solti/podium" {
		t.Fatal("YAML path not loaded")
	}
	t.Setenv("SOLTI_CLUSTER_STANDALONE_DATA_DIR", "/tmp/solti-config-fixture")
	if err := envconfig.Process("SOLTI_CLUSTER", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Standalone.DataDir != "/tmp/solti-config-fixture" {
		t.Fatal("environment path not loaded")
	}
}

func TestStandaloneDurabilityMustBeExplicit(t *testing.T) {
	cfg := cluster.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Standalone.DataDir = " "
	if err := cfg.Validate(); err == nil {
		t.Fatal("blank durable path accepted")
	}
	cfg.Standalone.Ephemeral = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOLTI_CLUSTER_STANDALONE_EPHEMERAL", "true")
	cfg = cluster.DefaultConfig()
	if err := envconfig.Process("SOLTI_CLUSTER", &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Standalone.Ephemeral {
		t.Fatal("explicit ephemeral environment option not loaded")
	}
}

func TestStandaloneSnapshotThresholdConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		want            int
		valid           bool
	}{
		{name: "default", want: cluster.DefaultStandaloneSnapshotThreshold, valid: true},
		{name: "yaml", yaml: "standalone:\n  snapshot_threshold: 512\n", want: 512, valid: true},
		{name: "env", env: "1024", want: 1024, valid: true},
		{name: "zero keeps the store's default", env: "0", want: 0, valid: true},
		{name: "negative", yaml: "standalone:\n  snapshot_threshold: -1\n", want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cluster.DefaultConfig()
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			if tc.env != "" {
				t.Setenv("SOLTI_CLUSTER_STANDALONE_SNAPSHOT_THRESHOLD", tc.env)
			}
			if err := envconfig.Process("SOLTI_CLUSTER", &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Standalone.SnapshotThreshold != tc.want {
				t.Fatalf("snapshot_threshold = %d, want %d", cfg.Standalone.SnapshotThreshold, tc.want)
			}
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, want valid %v", err, tc.valid)
			}
		})
	}
}

// The configuration's default threshold is the one the durable store keeps when
// it is given none.
func TestStandaloneSnapshotThresholdDefaultIsTheStores(t *testing.T) {
	if cluster.DefaultStandaloneSnapshotThreshold != durable.DefaultSnapshotThreshold {
		t.Fatalf("cluster default %d, durable default %d: they must agree", cluster.DefaultStandaloneSnapshotThreshold, durable.DefaultSnapshotThreshold)
	}
}

func TestDataDirIsThatOfTheSelectedBackend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   string
	}{
		{"standalone", func(*cluster.Config) {}, "/var/lib/solti/podium"},
		{"unset backend is standalone", func(c *cluster.Config) { c.Backend = "" }, "/var/lib/solti/podium"},
		{"ephemeral standalone has no directory", func(c *cluster.Config) { c.Standalone.Ephemeral = true }, ""},
		{"raft", func(c *cluster.Config) { c.Backend = cluster.BackendRaft }, "/var/lib/solti/raft"},
		{"ephemeral is a standalone option", func(c *cluster.Config) {
			c.Backend = cluster.BackendRaft
			c.Standalone.Ephemeral = true
		}, "/var/lib/solti/raft"},
		{"unknown backend", func(c *cluster.Config) { c.Backend = "other" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cluster.DefaultConfig()
			cfg.Standalone.DataDir = "/var/lib/solti/podium"
			cfg.Raft.DataDir = "/var/lib/solti/raft"
			tc.mutate(&cfg)
			if got := cfg.DataDir(); got != tc.want {
				t.Fatalf("DataDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEphemeralIsAStandaloneOption(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   bool
	}{
		{"durable standalone", func(*cluster.Config) {}, false},
		{"ephemeral standalone", func(c *cluster.Config) { c.Standalone.Ephemeral = true }, true},
		{"ephemeral with an unset backend", func(c *cluster.Config) {
			c.Backend = ""
			c.Standalone.Ephemeral = true
		}, true},
		{"raft ignores the flag", func(c *cluster.Config) {
			c.Backend = cluster.BackendRaft
			c.Standalone.Ephemeral = true
		}, false},
		{"unknown backend", func(c *cluster.Config) {
			c.Backend = "other"
			c.Standalone.Ephemeral = true
		}, false},
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
// 10.0.0.1:7000, its transport runs mutual TLS, and its static peers name all
// three nodes.
func raftConfig() cluster.Config {
	cfg := cluster.DefaultConfig()
	cfg.Backend = cluster.BackendRaft
	cfg.Raft = cluster.RaftConfig{NodeID: "n1", BindAddr: "10.0.0.1:7000", DataDir: "/var/lib/solti/raft", TLS: cluster.RaftTLSConfig{
		CertFile: "/etc/solti/raft/n1.crt",
		KeyFile:  "/etc/solti/raft/n1.key",
		CAFile:   "/etc/solti/raft/ca.crt",
	}}
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

// Standalone reads no raft or discovery setting, so invalid ones do not fail
// its validation.
func TestStandaloneIgnoresRaftConfig(t *testing.T) {
	for _, backend := range []cluster.Backend{"", cluster.BackendStandalone} {
		cfg := cluster.DefaultConfig()
		cfg.Backend = backend
		cfg.Raft = cluster.RaftConfig{BindAddr: "0.0.0.0:7000", TLS: cluster.RaftTLSConfig{CertFile: "n1.crt"}, AllowInsecureTransport: true}
		cfg.Discovery = cluster.DiscoveryConfig{Driver: "consul", Peers: []string{"=10.0.0.1:7000"}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("backend %q: Validate() = %v, want nil", backend, err)
		}
	}
}
