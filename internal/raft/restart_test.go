package raft

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"google.golang.org/protobuf/proto"
)

// openRestartProfile starts a one-node cluster from dir, a private data
// directory, with mutual TLS on a certificate of a CA of its own, and returns
// it once it leads, with its inner store and its hub.
func openRestartProfile(t *testing.T, dir string) (*Profile, *inmemory.Store, *event.Hub) {
	t.Helper()
	inner := inmemory.New()
	hub := event.NewHub(zerolog.Nop())
	self := []cluster.Peer{{ID: "restart-node", Address: "127.0.0.1:0"}}
	p, err := New(Config{NodeID: "restart-node", BindAddr: "127.0.0.1:0", DataDir: dir, ElectionTimeout: 50 * time.Millisecond, HeartbeatTimeout: 50 * time.Millisecond, TLS: newTestCA(t).issue(t, "127.0.0.1")}, inner, hub, discovery.NewStatic(self), zerolog.Nop())
	if err != nil {
		hub.Close()
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !p.Leadership().AmLeader() {
		if time.Now().After(deadline) {
			_ = p.Shutdown()
			hub.Close()
			t.Fatal("restart node did not become leader within two seconds")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p, inner, hub
}

func TestRaftCurrentStateRestoresFromLogAndSnapshot(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "command-log", true: "snapshot"}[compact], func(t *testing.T) {
			dir := privateTestDir(t)
			p, _, hub := openRestartProfile(t, dir)
			agent, err := model.NewAgent("restart-agent", "persisted", "http://agent")
			if err != nil {
				t.Fatal(err)
			}
			agent.SetOS("linux")
			agent.LabelAdd("env", "test")
			agent.SetCapabilities([]model.RunnerCapability{{Name: "echo", Labels: map[string]string{"tier": "test"}, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Shell"}}}})
			if err := p.Store().UpsertAgent(context.Background(), agent); err != nil {
				_ = p.Shutdown()
				hub.Close()
				t.Fatal(err)
			}
			if compact {
				if err := p.raft.Snapshot().Error(); err != nil {
					_ = p.Shutdown()
					hub.Close()
					t.Fatal(err)
				}
			}
			if err := p.Shutdown(); err != nil {
				hub.Close()
				t.Fatal(err)
			}
			hub.Close()
			restored, inner, restoredHub := openRestartProfile(t, dir)
			defer func() { _ = restored.Shutdown() }()
			defer restoredHub.Close()
			// Election precedes asynchronous FSM replay. A leader read barrier must
			// complete before bootstrap or restored reads can consume prior state.
			if err := cluster.AwaitLeaderReady(context.Background(), restored.Leadership()); err != nil {
				t.Fatal(err)
			}
			got, err := inner.GetAgent(context.Background(), agent.ID())
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(wire.AgentToProto(agent), wire.AgentToProto(got)) {
				t.Fatal("current-format state changed during startup preflight/replay")
			}
		})
	}
}

// identityErrors returns the ERROR lines in which a node's start reported that
// its stored configuration does not hold it.
func identityErrors(t *testing.T, logs *logBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range logLines(t, logs) {
		if line["level"] == "error" && line["message"] == identityMismatch {
			out = append(out, line)
		}
	}
	return out
}

// A node with raft state checks at start that its stored configuration holds
// it as its node_id at its advertise address, as its peers know it. A node
// restarted under another node_id, or at another address, starts all the same,
// and logs one ERROR that names its ID, its address and the stored servers, and
// says that a renamed or re-addressed node must be removed and added again with
// join: true. A node restarted as itself logs none, and neither does a node
// whose state holds no configuration yet, as a joining node's before the
// leader reaches it.
func TestRestartChecksTheNodeAgainstItsStoredConfiguration(t *testing.T) {
	ca := newTestCA(t)
	addrs := reserveLoopbackAddrs(t, 2)
	for _, tc := range []struct {
		name     string
		id, addr string // the node's node_id and advertise address at its restart
		logged   bool
	}{
		{"as itself", "n1", addrs[0], false},
		{"under another node_id", "n9", addrs[0], true},
		{"at another address", "n1", addrs[1], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newLoopbackNode(t, ca, "n1", addrs[0])
			t.Cleanup(n.stop)
			n.start(t, n.self(), oneNodeTimeouts)
			n.stop()
			logs := n.capture()
			n.start(t, n.self(), oneNodeTimeouts, func(c *Config) { c.NodeID, c.BindAddr = tc.id, tc.addr })
			lines := identityErrors(t, logs)
			if !tc.logged {
				if len(lines) != 0 {
					t.Fatalf("%s restarted as itself logged %v", n.id, lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("the restart logged %d identity ERROR lines, want one: %s", len(lines), logs.String())
			}
			line := lines[0]
			servers, _ := line["servers"].([]any)
			if line["node_id"] != tc.id || line["advertise_addr"] != tc.addr || len(servers) != 1 || servers[0] != "n1="+addrs[0] {
				t.Fatalf("the identity ERROR is %v, want node_id %s, advertise_addr %s and the servers [n1=%s]", line, tc.id, tc.addr, addrs[0])
			}
			message, _ := line["message"].(string)
			if !strings.Contains(message, "never elected") || !strings.Contains(message, "cluster.raft.join: true") {
				t.Fatalf("the identity ERROR %q does not say that the node is never elected, and how to rename or re-address it", message)
			}
			if n.profile == nil {
				t.Fatal("the node did not start")
			}
		})
	}

	t.Run("with state but no configuration yet", func(t *testing.T) {
		n := newLoopbackNode(t, ca, "n1", addrs[0])
		t.Cleanup(n.stop)
		if err := os.Mkdir(n.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		stable, err := boltdb.New(boltdb.Options{Path: filepath.Join(n.dir, "raft-stable.bolt")})
		if err != nil {
			t.Fatal(err)
		}
		// hashicorp/raft's key of the current term (keyCurrentTerm in v1.8.0): a
		// term alone is raft state, HasExistingState says.
		if err := stable.SetUint64([]byte("CurrentTerm"), 3); err != nil {
			t.Fatal(err)
		}
		if err := stable.Close(); err != nil {
			t.Fatal(err)
		}
		logs := n.capture()
		n.start(t, n.self(), oneNodeTimeouts)
		if lines := identityErrors(t, logs); len(lines) != 0 {
			t.Fatalf("a node whose state holds no configuration logged %v", lines)
		}
	})
}

// Unlike the explicit readiness test above, this exercises the first transaction
// directly after election. The store must establish its own applied-state barrier
// before the callback constructs a read view from restored committed state.
func TestRaftFirstTransactionAfterElectionReadsRestoredState(t *testing.T) {
	dir := privateTestDir(t)
	p, _, hub := openRestartProfile(t, dir)
	agent, err := model.NewAgent("first-tx-agent", "persisted", "http://agent")
	if err != nil {
		_ = p.Shutdown()
		hub.Close()
		t.Fatal(err)
	}
	if err := p.Store().UpsertAgent(context.Background(), agent); err != nil {
		_ = p.Shutdown()
		hub.Close()
		t.Fatal(err)
	}
	if err := p.Shutdown(); err != nil {
		hub.Close()
		t.Fatal(err)
	}
	hub.Close()
	restored, _, restoredHub := openRestartProfile(t, dir)
	defer func() { _ = restored.Shutdown() }()
	defer restoredHub.Close()
	// Deliberately no AwaitLeaderReady/Barrier before this first WithTx call.
	if err := restored.Store().WithTx(context.Background(), func(tx storage.Storage) error {
		got, err := tx.GetAgent(context.Background(), agent.ID())
		if err != nil {
			return err
		}
		if !proto.Equal(wire.AgentToProto(agent), wire.AgentToProto(got)) {
			t.Error("first post-election transaction saw changed state")
		}
		return nil
	}); err != nil {
		t.Fatalf("first post-election transaction saw stale state: %v", err)
	}
}
