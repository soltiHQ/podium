package raft

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// serverIDs returns the IDs of servers, in their order, each with its suffrage
// when it does not vote.
func serverIDs(servers []cluster.Server) []string {
	ids := make([]string, 0, len(servers))
	for _, s := range servers {
		id := s.ID
		if s.Suffrage != cluster.Voter {
			id += "(" + string(s.Suffrage) + ")"
		}
		ids = append(ids, id)
	}
	return ids
}

// readConfiguration reads n's latest configuration from its stores, and fails
// the test on an error.
func readConfiguration(t *testing.T, n *loopbackNode) cluster.Configuration {
	t.Helper()
	conf, err := n.profile.Admin().Configuration()
	if err != nil {
		t.Fatalf("%s: configuration: %v", n.id, err)
	}
	return conf
}

// The configuration a node reads from its own stores carries the index of the
// log entry that holds it: the bootstrap entry first, then, after each change,
// the index the change's IndexFuture gave, with the servers the change left.
// Followers read the same once they hold the entry.
func TestConfigurationIndexIsTheChangesIndex(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	leader := c.leader(t, 5*time.Second)
	admin := leader.profile.Admin()

	conf := readConfiguration(t, leader)
	if conf.Index != 1 || !slices.Equal(serverIDs(conf.Servers), []string{"n1", "n2", "n3"}) {
		t.Fatalf("bootstrapped configuration %+v, want n1, n2 and n3 voters at index 1, the bootstrap entry", conf)
	}

	var follower *loopbackNode
	for _, n := range c.nodes {
		if n != leader {
			follower = n
		}
	}
	removed, err := admin.RemoveServer(follower.id, conf.Index)
	if err != nil {
		t.Fatalf("remove %s: %v", follower.id, err)
	}
	conf = readConfiguration(t, leader)
	if conf.Index != removed || slices.Contains(serverIDs(conf.Servers), follower.id) || len(conf.Servers) != 2 {
		t.Fatalf("after the removal of %s at index %d: %+v", follower.id, removed, conf)
	}

	added, err := admin.AddVoter(follower.id, follower.addr, conf.Index)
	if err != nil {
		t.Fatalf("add %s back: %v", follower.id, err)
	}
	conf = readConfiguration(t, leader)
	if conf.Index != added || added <= removed || len(conf.Servers) != 3 {
		t.Fatalf("after %s was added back at index %d: %+v", follower.id, added, conf)
	}
	back := conf.Servers[len(conf.Servers)-1]
	if back != (cluster.Server{ID: follower.id, Address: follower.addr, Suffrage: cluster.Voter}) {
		t.Fatalf("%s was added back as %+v", follower.id, back)
	}
	for _, n := range c.nodes {
		waitFor(t, 5*time.Second, n.id+" reads the configuration at index "+fmt.Sprint(added), func() bool {
			got, err := n.profile.Admin().Configuration()
			return err == nil && got.Index == added && slices.Equal(got.Servers, conf.Servers)
		})
	}
}

// A snapshot that compacts the log past the entry of the latest configuration
// keeps the configuration and its index: the node then reads them in the
// snapshot, and a change against that index applies.
func TestConfigurationIndexSurvivesCompaction(t *testing.T) {
	const threshold, trailing = 10, 4
	n := newLoopbackNode(t, newTestCA(t), "n1", reserveLoopbackAddrs(t, 1)[0])
	t.Cleanup(n.stop)
	n.start(t, n.self(), oneNodeTimeouts, func(c *Config) {
		c.SnapshotInterval, c.SnapshotThreshold, c.TrailingLogs = 25*time.Millisecond, threshold, trailing
	})
	waitFor(t, 5*time.Second, "n1 leads", n.profile.leadership.AmLeader)
	admin := n.profile.Admin()

	// A change that keeps the one voter, n1 at its own address, writes an entry
	// of its own.
	index, err := admin.AddVoter(n.id, n.addr, readConfiguration(t, n).Index)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := range 3 * threshold {
		agent, err := model.NewAgent(fmt.Sprintf("a%d", i), "agent", "http://agent")
		if err != nil {
			t.Fatal(err)
		}
		if err := n.profile.Store().UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 10*time.Second, "a snapshot compacts the log past the configuration's entry", func() bool {
		first, err := n.profile.logStore.FirstIndex()
		return err == nil && first > index
	})

	conf := readConfiguration(t, n)
	if conf.Index != index || !slices.Equal(serverIDs(conf.Servers), []string{"n1"}) {
		t.Fatalf("after the compaction: %+v, want n1 at index %d, the snapshot's", conf, index)
	}
	if _, err := admin.AddVoter(n.id, n.addr, conf.Index); err != nil {
		t.Fatalf("a change against the index read from the snapshot: %v", err)
	}
}

// A change against an index that is no longer the latest configuration's
// changes nothing. hashicorp/raft v1.8.0 refuses it as "configuration changed
// since ...", which has no sentinel: Admin gives it as
// cluster.ErrConfigurationChanged by that text, a conflict, so a newer
// hashicorp/raft that words it otherwise fails this test.
func TestStaleConfigurationIndexConflicts(t *testing.T) {
	n := newLoopbackNode(t, newTestCA(t), "n1", reserveLoopbackAddrs(t, 1)[0])
	t.Cleanup(n.stop)
	n.start(t, n.self(), oneNodeTimeouts)
	waitFor(t, 5*time.Second, "n1 leads", n.profile.leadership.AmLeader)
	admin := n.profile.Admin()

	read := readConfiguration(t, n)
	changed, err := admin.AddVoter(n.id, n.addr, read.Index)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func() (uint64, error)
	}{
		{"add", func() (uint64, error) { return admin.AddVoter("n9", "127.0.0.1:1", read.Index) }},
		{"remove", func() (uint64, error) { return admin.RemoveServer(n.id, read.Index) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.change()
			if !errors.Is(err, cluster.ErrConfigurationChanged) || !strings.HasPrefix(err.Error(), "configuration changed since ") {
				t.Fatalf("a change at the stale index %d: %v, want %v with hashicorp/raft's text", read.Index, err, cluster.ErrConfigurationChanged)
			}
			if kind := errkind.Classify(err); kind != errkind.Conflict {
				t.Fatalf("errkind.Classify(%v) = %v, want Conflict", err, kind)
			}
			if now := readConfiguration(t, n); now.Index != changed || !slices.Equal(serverIDs(now.Servers), []string{"n1"}) {
				t.Fatalf("the refused change left %+v, want n1 at index %d", now, changed)
			}
		})
	}
}

// Probe dials an address as the transport dials a peer: it succeeds where a node
// with a certificate of the cluster CA for the dialed host answers, and fails
// with cluster.ErrPeerUnreachable, a failed precondition, where nothing listens,
// what answers presents a certificate of another CA, one for another host, or
// none, or refuses the certificate of the node that probes. A probe of a node
// ends cleanly: the node logs no error for it.
func TestProbeDialsAsRaftDoes(t *testing.T) {
	c := loopbackNodes(t, "n1", "n2")
	prober, target := c.nodes[0], c.nodes[1]
	logs := target.capture()
	prober.start(t, prober.self(), oneNodeTimeouts)
	target.start(t, target.self(), oneNodeTimeouts)
	admin := prober.profile.Admin()
	ctx := context.Background()

	if err := admin.Probe(ctx, target.addr); err != nil {
		t.Fatalf("probe of %s, a node of the cluster CA: %v", target.id, err)
	}
	if out := logs.String(); strings.Contains(out, `"level":"error"`) {
		t.Fatalf("%s logged an error for the probe: %s", target.id, out)
	}

	other := newTestCA(t)
	// server is a peer with the certificate of files, which accepts clients of
	// trusted only.
	server := func(files cluster.RaftTLSConfig, trusted *testCA) string {
		return peerServer(t, &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{certOf(t, files)},
			ClientCAs:    trusted.pool(),
			ClientAuth:   tls.RequireAndVerifyClientCert,
		})
	}
	for _, tc := range []struct {
		name, addr string
	}{
		{"nothing listens", reserveLoopbackAddrs(t, 1)[0]},
		{"a certificate of another CA", server(other.issue(t, "127.0.0.1"), c.ca)},
		{"a certificate for another host", server(c.ca.issue(t, "10.0.0.9"), c.ca)},
		// Over TLS 1.3 a client's handshake ends before the server checks the
		// client's certificate: the probe reads the peer's refusal after it.
		{"a peer that refuses this node's certificate", server(c.ca.issue(t, "127.0.0.1"), other)},
		{"no TLS", peerServer(t, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := admin.Probe(ctx, tc.addr)
			if !errors.Is(err, cluster.ErrPeerUnreachable) || !strings.Contains(err.Error(), tc.addr) {
				t.Fatalf("probe of %s: %v, want %v naming the address", tc.addr, err, cluster.ErrPeerUnreachable)
			}
			if kind := errkind.Classify(err); kind != errkind.FailedPrecondition {
				t.Fatalf("errkind.Classify(%v) = %v, want FailedPrecondition", err, kind)
			}
		})
	}
}

// A node that joins with no raft state bootstraps nothing: it asks no
// discovery, logs one INFO line that it waits to be added, with its ID and
// address, and holds no configuration until the leader adds it, and then
// catches up the writes made before. Once it holds raft state, join changes
// nothing: it restarts into its cluster from its own log, and logs no such
// line.
func TestJoiningNodeWaitsToBeAdded(t *testing.T) {
	c := loopbackNodes(t, "n1", "n2")
	first, joiner := c.nodes[0], c.nodes[1]
	first.start(t, first.self())
	waitFor(t, 5*time.Second, "n1 leads", first.profile.leadership.AmLeader)
	ctx := context.Background()
	agent, err := model.NewAgent("written-before-the-join", "agent", "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.profile.Store().UpsertAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}

	join := func(c *Config) { c.Join = true }
	logs := joiner.capture()
	// waits returns the INFO lines in which the joiner said it waits to be added.
	waits := func() []map[string]any {
		var lines []map[string]any
		for _, line := range logLines(t, logs) {
			message, _ := line["message"].(string)
			if line["level"] == "info" && strings.HasPrefix(message, "cluster.raft.join is true and the node has no raft state") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	joiner.start(t, failingDiscovery{}, join)
	if lines := waits(); len(lines) != 1 || lines[0]["node_id"] != joiner.id || lines[0]["advertise_addr"] != joiner.addr {
		t.Fatalf("%s logged %v, want one INFO line that it waits to be added, with its ID and address", joiner.id, lines)
	}
	if servers := joiner.profile.Admin().Servers(); len(servers) != 0 {
		t.Fatalf("%s joined with the configuration %v, want none until the leader adds it", joiner.id, servers)
	}
	if last := joiner.profile.Admin().LastIndex(); last != 0 {
		t.Fatalf("%s joined with a log up to %d, want an empty one", joiner.id, last)
	}

	conf := readConfiguration(t, first)
	if _, err := first.profile.Admin().AddVoter(joiner.id, joiner.addr, conf.Index); err != nil {
		t.Fatalf("add %s: %v", joiner.id, err)
	}
	waitFor(t, 5*time.Second, joiner.id+" holds the write made before it joined", func() bool {
		_, err := joiner.profile.Store().GetAgent(ctx, agent.ID())
		return err == nil
	})
	want := []string{"n1", "n2"}
	if got := serverIDs(joiner.profile.Admin().Servers()); !slices.Equal(got, want) {
		t.Fatalf("%s holds the configuration %v, want %v", joiner.id, got, want)
	}

	joiner.stop()
	joiner.start(t, failingDiscovery{}, join)
	if lines := waits(); len(lines) != 1 {
		t.Fatalf("%s logged %d INFO lines that it waits to be added once it restarted with raft state, want the one of its first start", joiner.id, len(lines))
	}
	waitFor(t, 10*time.Second, joiner.id+" follows a leader again", func() bool {
		id, _ := joiner.profile.Admin().Leader()
		return id != ""
	})
	if got := serverIDs(joiner.profile.Admin().Servers()); !slices.Equal(got, want) {
		t.Fatalf("%s restarted with the configuration %v, want %v", joiner.id, got, want)
	}
}
