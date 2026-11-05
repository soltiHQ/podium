package raft

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// Every node, the leader and its followers, lists the members of its raft
// configuration: each ID with its address as the configuration writes it, in
// the configuration's order, which the bootstrap sorted by ID.
func TestLeadershipListsTheMembers(t *testing.T) {
	c := newLoopbackCluster(t, "n3", "n1", "n2")
	c.leader(t, 5*time.Second)
	var want []cluster.Peer
	for _, n := range c.nodes {
		want = append(want, cluster.Peer{ID: n.id, Address: n.addr})
	}
	slices.SortFunc(want, func(a, b cluster.Peer) int { return strings.Compare(a.ID, b.ID) })
	for _, n := range c.nodes {
		membership, ok := n.profile.Leadership().(cluster.Membership)
		if !ok {
			t.Fatalf("the leadership of %s lists no members", n.id)
		}
		if got := membership.Members(); !slices.Equal(got, want) {
			t.Fatalf("%s lists the members %v, want %v", n.id, got, want)
		}
	}
}
