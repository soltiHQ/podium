package raft

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// loopbackTimeout is the election and heartbeat timeout of loopback nodes. It
// leaves room for a slow race-detector run, in which a shorter lease can lapse
// between a leader's election and its first write.
const loopbackTimeout = 250 * time.Millisecond

// loopbackCluster runs real nodes, each opened by New on its own loopback TCP
// port. The ports are reserved before any node starts, so every node lists all
// of them as id=127.0.0.1:port entries of cluster.discovery.peers.
type loopbackCluster struct {
	nodes []*loopbackNode
}

// loopbackNode is one node of a loopbackCluster.
type loopbackNode struct {
	id   string
	addr string // 127.0.0.1:port, its bind and advertise address
	dir  string // its data directory, kept across restarts

	profile *Profile // nil while the node is stopped
	hub     *event.Hub
}

// newLoopbackCluster starts one node per ID, each bootstrapping from
// [loopbackCluster.discovery], and stops them all when the test ends.
func newLoopbackCluster(t *testing.T, ids ...string) *loopbackCluster {
	t.Helper()
	c := &loopbackCluster{}
	for i, addr := range reserveLoopbackAddrs(t, len(ids)) {
		c.nodes = append(c.nodes, &loopbackNode{id: ids[i], addr: addr, dir: t.TempDir()})
	}
	t.Cleanup(func() {
		for _, n := range c.nodes {
			n.stop()
		}
	})
	for _, n := range c.nodes {
		n.start(t, c.discovery(t))
	}
	return c
}

// discovery lists every node of c as the static driver does, from
// id=host:port entries of cluster.discovery.peers.
func (c *loopbackCluster) discovery(t *testing.T) cluster.Discovery {
	t.Helper()
	entries := make([]string, 0, len(c.nodes))
	for _, n := range c.nodes {
		entries = append(entries, n.id+"="+n.addr)
	}
	disco, err := discovery.New(cluster.DiscoveryConfig{Peers: entries})
	if err != nil {
		t.Fatal(err)
	}
	return disco
}

// leader waits until exactly one running node leads and has applied every
// committed entry, and returns it.
func (c *loopbackCluster) leader(t *testing.T, timeout time.Duration) *loopbackNode {
	t.Helper()
	var leader *loopbackNode
	waitFor(t, timeout, "one ready leader", func() bool {
		leader = nil
		for _, n := range c.nodes {
			if n.profile == nil || !n.profile.leadership.AmLeader() {
				continue
			}
			if leader != nil {
				return false
			}
			leader = n
		}
		if leader == nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return leader.profile.leadership.WaitReady(ctx) == nil
	})
	return leader
}

// write runs fn on the leader's store and, when leadership moves before the
// write commits, runs it again on the next leader until timeout. fn must
// tolerate a second run: a write whose leader stepped down may still commit.
func (c *loopbackCluster) write(t *testing.T, timeout time.Duration, fn func(storage.Storage) error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := fn(c.leader(t, time.Until(deadline)).profile.Store())
		switch {
		case err == nil:
			return
		case !errors.Is(err, hraft.ErrNotLeader) && !errors.Is(err, hraft.ErrLeadershipLost):
			t.Fatalf("write: %v", err)
		case time.Now().After(deadline):
			t.Fatalf("write: %v, still after %s", err, timeout)
		}
	}
}

// start opens n from its data directory: a first start bootstraps from disco,
// a restart reloads the node's own log. Each opt edits the node's Config
// before New gets it.
func (n *loopbackNode) start(t *testing.T, disco cluster.Discovery, opts ...func(*Config)) {
	t.Helper()
	cfg := Config{
		NodeID:           n.id,
		BindAddr:         n.addr,
		DataDir:          n.dir,
		ElectionTimeout:  loopbackTimeout,
		HeartbeatTimeout: loopbackTimeout,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	hub := event.NewHub(zerolog.Nop())
	p, err := New(cfg, inmemory.New(), hub, disco, zerolog.Nop())
	if err != nil {
		hub.Close()
		t.Fatalf("start %s: %v", n.id, err)
	}
	n.profile, n.hub = p, hub
}

// stop shuts n down and keeps its data directory for a restart.
func (n *loopbackNode) stop() {
	if n.profile == nil {
		return
	}
	_ = n.profile.Shutdown()
	n.hub.Close()
	n.profile, n.hub = nil, nil
}

// reserveLoopbackAddrs returns count distinct 127.0.0.1 addresses on ports the
// kernel picked. Its listeners are closed when it returns, so nodes can bind
// the ports; another process may take one in between, a race tests accept.
func reserveLoopbackAddrs(t *testing.T, count int) []string {
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

// waitFor polls cond every 10ms until it holds, and fails the test when it
// still does not after timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, timeout)
		}
		<-tick.C
	}
}
