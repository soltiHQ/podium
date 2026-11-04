package raft

import (
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// Zero election and heartbeat timeouts keep hashicorp/raft's defaults of one
// second, set ones replace them, and an election timeout below the heartbeat
// timeout fails the start.
func TestRaftTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name                string
		election, heartbeat time.Duration
		wantElection        time.Duration
		wantHeartbeat       time.Duration
	}{
		{"zero keeps the defaults", 0, 0, time.Second, time.Second},
		{"set", 400 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &loopbackNode{id: "n1", addr: reserveLoopbackAddrs(t, 1)[0], dir: t.TempDir()}
			t.Cleanup(n.stop)
			n.start(t, discovery.NewStatic([]cluster.Peer{{ID: n.id, Address: n.addr}}), func(c *Config) {
				c.ElectionTimeout, c.HeartbeatTimeout = tc.election, tc.heartbeat
			})
			got := n.profile.raft.ReloadableConfig()
			if got.ElectionTimeout != tc.wantElection || got.HeartbeatTimeout != tc.wantHeartbeat {
				t.Fatalf("election %v, heartbeat %v; want %v, %v", got.ElectionTimeout, got.HeartbeatTimeout, tc.wantElection, tc.wantHeartbeat)
			}
		})
	}

	t.Run("election below heartbeat", func(t *testing.T) {
		addr := reserveLoopbackAddrs(t, 1)[0]
		hub := event.NewHub(zerolog.Nop())
		t.Cleanup(hub.Close)
		p, err := New(Config{
			NodeID:           "n1",
			BindAddr:         addr,
			DataDir:          t.TempDir(),
			ElectionTimeout:  100 * time.Millisecond,
			HeartbeatTimeout: 200 * time.Millisecond,
		}, inmemory.New(), hub, discovery.NewStatic([]cluster.Peer{{ID: "n1", Address: addr}}), zerolog.Nop())
		if err == nil || !strings.Contains(err.Error(), "ElectionTimeout") {
			if p != nil {
				_ = p.Shutdown()
			}
			t.Fatalf("New() = %v, want the election timeout refused", err)
		}
	})
}
