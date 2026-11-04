package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	raftpkg "github.com/soltiHQ/control-plane/internal/raft"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/durable"
)

func TestBuildStandaloneDurableAndEphemeral(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "ephemeral", true: "durable"}[persistent], func(t *testing.T) {
			cfg := cluster.DefaultConfig()
			cfg.Standalone.Ephemeral = !persistent
			if persistent {
				cfg.Standalone.DataDir = filepath.Join(t.TempDir(), "state")
			}
			store, leader, hub, shutdown, err := buildCluster(cfg, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			if !leader.AmLeader() {
				t.Fatal("standalone must be leader")
			}
			_, isDurable := store.(*durable.Store)
			if isDurable != persistent {
				t.Fatal("wrong standalone backend")
			}
			if _, ok := store.(storage.Scanner); !ok {
				t.Fatalf("%T is not a storage.Scanner: every scan of it would page", store)
			}
			ctx := context.Background()
			agent, _ := model.NewAgent("a1", "fixture", "http://fixture")
			if err := store.UpsertAgent(ctx, agent); err != nil {
				t.Fatal(err)
			}
			hub.Close()
			if shutdown != nil {
				shutdown()
			}
			store, _, hub, shutdown, err = buildCluster(cfg, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			defer hub.Close()
			if shutdown != nil {
				defer shutdown()
			}
			_, err = store.GetAgent(ctx, "a1")
			if persistent && err != nil {
				t.Fatalf("durable restart lost state: %v", err)
			}
			if !persistent && !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("ephemeral restart: %v", err)
			}
		})
	}
}

// The first start on a single-blob file logs the migration once, at WARN, with the
// data directory, the new format and the fact that older versions refuse the file.
// The next start finds a file in format 2 and logs no such warning.
func TestBuildClusterLogsTheMigrationOnce(t *testing.T) {
	fixture, err := os.ReadFile("../storage/durable/testdata/single-blob.db")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, durable.FileName), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cluster.DefaultConfig()
	cfg.Standalone.DataDir = dir

	for _, start := range []struct {
		name  string
		warns int
	}{{"first start", 1}, {"second start", 0}} {
		var logs bytes.Buffer
		_, _, hub, shutdown, err := buildCluster(cfg, zerolog.New(&logs))
		if err != nil {
			t.Fatalf("%s: %v", start.name, err)
		}
		hub.Close()
		shutdown()

		out := logs.String()
		if warns := strings.Count(out, `"level":"warn"`); warns != start.warns {
			t.Fatalf("%s logged %d warnings, want %d: %s", start.name, warns, start.warns, out)
		}
		if start.warns == 0 {
			continue
		}
		for _, want := range []string{dir, "single-blob", "format 2", "older versions refuse the migrated file"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s: the warning lacks %q: %s", start.name, want, out)
			}
		}
	}
}

func TestBuildClusterRefusesImplicitEphemeral(t *testing.T) {
	cfg := cluster.DefaultConfig()
	cfg.Standalone.DataDir = ""
	if _, _, _, _, err := buildCluster(cfg, zerolog.Nop()); err == nil {
		t.Fatal("blank data directory silently selected ephemeral state")
	}
}

// The raft timeouts are configured in milliseconds; zero or less leaves raft.New
// its own default.
func TestMsToDuration(t *testing.T) {
	for ms, want := range map[int]time.Duration{-1: 0, 0: 0, 250: 250 * time.Millisecond} {
		if got := msToDuration(ms); got != want {
			t.Fatalf("msToDuration(%d) = %v, want %v", ms, got, want)
		}
	}
}

// The raft node gets every setting of the raft configuration, the compaction
// settings with it, and a number of log entries of zero or less is left to raft.New.
func TestRaftNodeConfig(t *testing.T) {
	files := cluster.RaftTLSConfig{CertFile: "n1.crt", KeyFile: "n1.key", CAFile: "ca.crt"}
	got := raftNodeConfig(cluster.RaftConfig{
		NodeID: "n1", BindAddr: "0.0.0.0:7000", AdvertiseAddr: "10.0.0.1:7000", DataDir: "/var/lib/solti/raft",
		ElectionTimeoutMs: 1000, HeartbeatTimeoutMs: 500,
		SnapshotInterval: 30 * time.Second, SnapshotThreshold: 4096, TrailingLogs: 2048,
		TLS: files, AllowInsecureTransport: true,
	})
	want := raftpkg.Config{
		NodeID: "n1", BindAddr: "0.0.0.0:7000", AdvertiseAddr: "10.0.0.1:7000", DataDir: "/var/lib/solti/raft",
		ElectionTimeout: time.Second, HeartbeatTimeout: 500 * time.Millisecond,
		SnapshotInterval: 30 * time.Second, SnapshotThreshold: 4096, TrailingLogs: 2048,
		TLS: files, AllowInsecureTransport: true,
	}
	if got != want {
		t.Fatalf("raftNodeConfig() = %+v, want %+v", got, want)
	}
	for n, want := range map[int]uint64{-1: 0, 0: 0, 1: 1, 8192: 8192} {
		if got := entries(n); got != want {
			t.Fatalf("entries(%d) = %d, want %d", n, got, want)
		}
	}
}

// loopbackAddrs returns count distinct 127.0.0.1 addresses on ports the kernel
// picked, free again when it returns.
func loopbackAddrs(t *testing.T, count int) []string {
	t.Helper()
	addrs := make([]string, 0, count)
	listeners := make([]net.Listener, 0, count)
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for range count {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, l)
		addrs = append(addrs, l.Addr().String())
	}
	return addrs
}

// unopenedRaftConfig is a raft configuration that passes validation, for node
// n1 of three on loopback ports, with a data directory that does not exist yet.
func unopenedRaftConfig(t *testing.T) (cfg cluster.Config, dir string) {
	t.Helper()
	addrs := loopbackAddrs(t, 3)
	dir = filepath.Join(t.TempDir(), "raft")
	cfg = cluster.DefaultConfig()
	cfg.Backend = cluster.BackendRaft
	// Validation only checks that the TLS files are named.
	tls := cluster.RaftTLSConfig{CertFile: "n1.crt", KeyFile: "n1.key", CAFile: "ca.crt"}
	cfg.Raft = cluster.RaftConfig{NodeID: "n1", BindAddr: addrs[0], DataDir: dir, ElectionTimeoutMs: 50, HeartbeatTimeoutMs: 50, TLS: tls}
	cfg.Discovery.Peers = []string{"n1=" + addrs[0], "n2=" + addrs[1], "n3=" + addrs[2]}
	return cfg, dir
}

// A compaction setting that the configuration or hashicorp/raft refuses, a
// snapshot interval under its minimum included, fails buildCluster before anything
// is opened, like any other invalid raft setting: the data directory is never
// created, where hashicorp/raft would refuse the interval only when it starts.
func TestRaftCompactionValidatedBeforeStoreOpens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.RaftConfig)
		key    string // the setting the error names; the TLS files of the test would fail the node later, but not by name
	}{
		{"a negative snapshot interval", func(r *cluster.RaftConfig) { r.SnapshotInterval = -time.Second }, "raft.snapshot_interval"},
		{"a snapshot interval under the minimum", func(r *cluster.RaftConfig) { r.SnapshotInterval = time.Millisecond }, "raft.snapshot_interval"},
		{"a negative snapshot threshold", func(r *cluster.RaftConfig) { r.SnapshotThreshold = -1 }, "raft.snapshot_threshold"},
		{"negative trailing logs", func(r *cluster.RaftConfig) { r.TrailingLogs = -1 }, "raft.trailing_logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, dir := unopenedRaftConfig(t)
			tc.mutate(&cfg.Raft)
			_, _, hub, shutdown, err := buildCluster(cfg, zerolog.Nop())
			if err == nil {
				hub.Close()
				shutdown()
				t.Fatal("buildCluster accepted the configuration")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("buildCluster() = %v, want an error that names %s", err, tc.key)
			}
			if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the data directory exists after a refused configuration: %v", err)
			}
		})
	}
}

// An invalid raft configuration fails buildCluster before anything is opened:
// its data directory is never created.
func TestRaftConfigValidatedBeforeStoreOpens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   error
	}{
		{"neither TLS nor allow_insecure_transport", func(c *cluster.Config) { c.Raft.TLS = cluster.RaftTLSConfig{} }, cluster.ErrRaftTLSRequired},
		{"no node ID", func(c *cluster.Config) { c.Raft.NodeID = "" }, cluster.ErrRaftRequired},
		{"no bind address", func(c *cluster.Config) { c.Raft.BindAddr = "" }, cluster.ErrRaftRequired},
		{"no data directory", func(c *cluster.Config) { c.Raft.DataDir = "" }, cluster.ErrRaftRequired},
		{"every interface without advertise_addr", func(c *cluster.Config) {
			c.Raft.BindAddr = "0.0.0.0" + c.Raft.BindAddr[strings.LastIndex(c.Raft.BindAddr, ":"):]
		}, cluster.ErrAdvertiseRequired},
		{"bare peers with a named node", func(c *cluster.Config) {
			for i, entry := range c.Discovery.Peers {
				_, c.Discovery.Peers[i], _ = strings.Cut(entry, "=")
			}
		}, cluster.ErrNodeIDNotAdvertise},
		{"peers without this node", func(c *cluster.Config) { c.Discovery.Peers = c.Discovery.Peers[1:] }, cluster.ErrNodeNotListed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, dir := unopenedRaftConfig(t)
			tc.mutate(&cfg)

			_, _, hub, shutdown, err := buildCluster(cfg, zerolog.Nop())
			if err == nil {
				hub.Close()
				shutdown()
				t.Fatalf("buildCluster accepted the configuration, want %v", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("buildCluster() = %v, want %v", err, tc.want)
			}
			if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the data directory exists after a refused configuration: %v", err)
			}
		})
	}
}
