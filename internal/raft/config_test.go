package raft

import (
	"strings"
	"testing"
	"time"
)

// Zero election and heartbeat timeouts keep hashicorp/raft's defaults of one
// second, set ones replace them, and an election timeout below the heartbeat
// timeout fails the start.
func TestRaftTimeouts(t *testing.T) {
	ca := newTestCA(t)
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
			n := newLoopbackNode(t, ca, "n1", reserveLoopbackAddrs(t, 1)[0])
			t.Cleanup(n.stop)
			n.start(t, n.self(), func(c *Config) {
				c.ElectionTimeout, c.HeartbeatTimeout = tc.election, tc.heartbeat
			})
			got := n.profile.raft.ReloadableConfig()
			if got.ElectionTimeout != tc.wantElection || got.HeartbeatTimeout != tc.wantHeartbeat {
				t.Fatalf("election %v, heartbeat %v; want %v, %v", got.ElectionTimeout, got.HeartbeatTimeout, tc.wantElection, tc.wantHeartbeat)
			}
		})
	}

	t.Run("election below heartbeat", func(t *testing.T) {
		n := newLoopbackNode(t, ca, "n1", reserveLoopbackAddrs(t, 1)[0])
		t.Cleanup(n.stop)
		err := n.open(n.self(), func(c *Config) {
			c.ElectionTimeout, c.HeartbeatTimeout = 100*time.Millisecond, 200*time.Millisecond
		})
		if err == nil || !strings.Contains(err.Error(), "ElectionTimeout") {
			t.Fatalf("New() = %v, want the election timeout refused", err)
		}
	})
}
