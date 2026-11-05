package cluster_test

import (
	"testing"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// The leader serves a listener at the host of its raft address, as written, and
// the port every replica serves the listener on: an IPv6 host keeps its
// brackets, a name stays a name. Without a leader or a port there is no address.
func TestLeaderAddr(t *testing.T) {
	for _, tc := range []struct {
		name, leader string
		port         int
		want         string
	}{
		{"an IPv4 leader", "10.0.0.1:7000", 50051, "10.0.0.1:50051"},
		{"an IPv6 leader", "[fd00::1]:7000", 50051, "[fd00::1]:50051"},
		{"a leader known by its host name", "podium-0.podium:7000", 8080, "podium-0.podium:8080"},
		{"a host without a port", "leader-host", 8082, "leader-host:8082"},
		{"no leader", "", 50051, ""},
		{"no port", "10.0.0.1:7000", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cluster.LeaderAddr(tc.leader, tc.port); got != tc.want {
				t.Fatalf("LeaderAddr(%q, %d) = %q, want %q", tc.leader, tc.port, got, tc.want)
			}
		})
	}
}
