package raft

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// errDiscoveryDown is the error of every failingDiscovery lookup.
var errDiscoveryDown = errors.New("discovery is down")

// failingDiscovery is a discovery whose every lookup fails.
type failingDiscovery struct{}

func (failingDiscovery) Peers(context.Context) ([]cluster.Peer, error) { return nil, errDiscoveryDown }
func (failingDiscovery) Watch(context.Context) <-chan []cluster.Peer   { return nil }

// dnsAnswers is a resolver that answers every lookup with the hosts of its
// peers, in their order.
type dnsAnswers []cluster.Peer

func (a dnsAnswers) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	ips := make([]net.IPAddr, 0, len(a))
	for _, p := range a {
		host, _, err := net.SplitHostPort(p.Address)
		if err != nil {
			return nil, err
		}
		ips = append(ips, net.IPAddr{IP: net.ParseIP(host)})
	}
	return ips, nil
}

// sortedServers returns peers as voters sorted by ID: the configuration every
// node of a new cluster bootstraps with.
func sortedServers(peers []cluster.Peer) []hraft.Server {
	servers := make([]hraft.Server, 0, len(peers))
	for _, p := range peers {
		servers = append(servers, hraft.Server{Suffrage: hraft.Voter, ID: hraft.ServerID(p.ID), Address: hraft.ServerAddress(p.Address)})
	}
	slices.SortFunc(servers, func(a, b hraft.Server) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return servers
}

// configuration returns the servers of n's current raft configuration.
func configuration(t *testing.T, n *loopbackNode) []hraft.Server {
	t.Helper()
	f := n.profile.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		t.Fatalf("%s: configuration: %v", n.id, err)
	}
	return f.Configuration().Servers
}

// Three nodes named n1, n2 and n3, each listing all three as id=host:port
// peers, bootstrap one configuration, elect a leader within 5 s and commit a
// write that every node applies.
func TestThreeNodesWithNamedIDsElectLeader(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	c.leader(t, 5*time.Second)

	ctx := context.Background()
	agent, err := model.NewAgent("named-agent", "replicated", "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	c.write(t, 5*time.Second, func(s storage.Storage) error { return s.UpsertAgent(ctx, agent) })
	peers := make([]cluster.Peer, 0, len(c.nodes))
	for _, n := range c.nodes {
		peers = append(peers, cluster.Peer{ID: n.id, Address: n.addr})
	}
	want := sortedServers(peers)
	for _, n := range c.nodes {
		waitFor(t, 5*time.Second, n.id+" applies the write", func() bool {
			_, err := n.profile.Store().GetAgent(ctx, agent.ID())
			return err == nil
		})
		if got := configuration(t, n); !slices.Equal(got, want) {
			t.Fatalf("%s bootstrapped %v, want %v", n.id, got, want)
		}
	}
}

// Every node derives the same server list, sorted by ID, whatever order its
// discovery reports the peers in: named peers, peers named by their address,
// and DNS answers.
func TestAllNodesDeriveTheSameServerList(t *testing.T) {
	named := []cluster.Peer{
		{ID: "n2", Address: "10.0.0.1:7000"},
		{ID: "n3", Address: "10.0.0.2:7000"},
		{ID: "n1", Address: "10.0.0.3:7000"},
	}
	byAddress := []cluster.Peer{
		{ID: "10.0.0.2:7000", Address: "10.0.0.2:7000"},
		{ID: "10.0.0.10:7000", Address: "10.0.0.10:7000"},
		{ID: "10.0.0.1:7000", Address: "10.0.0.1:7000"},
	}
	// rotated is what the discovery of the i-th node reports.
	rotated := func(peers []cluster.Peer, i int) []cluster.Peer {
		return append(slices.Clone(peers[i:]), peers[:i]...)
	}
	for _, tc := range []struct {
		name  string
		peers []cluster.Peer
		disco func(t *testing.T, i int) cluster.Discovery
	}{
		{"named peers", named, func(_ *testing.T, i int) cluster.Discovery {
			return discovery.NewStatic(rotated(named, i))
		}},
		{"peers named by their address", byAddress, func(_ *testing.T, i int) cluster.Discovery {
			return discovery.NewStatic(rotated(byAddress, i))
		}},
		{"dns", byAddress, func(t *testing.T, i int) cluster.Discovery {
			d, err := discovery.NewDNS("podium.svc", 7000, discovery.WithDNSResolver(dnsAnswers(rotated(byAddress, i))))
			if err != nil {
				t.Fatal(err)
			}
			return d
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := sortedServers(tc.peers)
			for i, self := range tc.peers {
				got, err := bootstrapServers(Config{NodeID: self.ID}, hraft.ServerAddress(self.Address), tc.disco(t, i))
				if err != nil {
					t.Fatalf("node %s: %v", self.ID, err)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("node %s derived %v, want %v", self.ID, got, want)
				}
			}
		})
	}
}

// A first start fails when discovery fails, or when the peers name an ID or an
// address twice, DNS answers included, or do not list this node as its node ID
// at its advertise address, as when the node advertises a host name and DNS
// answers with IPs.
func TestBootstrapRefusesInconsistentPeers(t *testing.T) {
	const self, addr = "n1", "10.0.0.1:7000"
	others := func(more ...cluster.Peer) cluster.Discovery {
		return discovery.NewStatic(append([]cluster.Peer{
			{ID: "n2", Address: "10.0.0.2:7000"},
			{ID: "n3", Address: "10.0.0.3:7000"},
		}, more...))
	}
	dns, err := discovery.NewDNS("podium.svc", 7000, discovery.WithDNSResolver(dnsAnswers{{Address: "10.0.0.1:7000"}, {Address: "10.0.0.2:7000"}}))
	if err != nil {
		t.Fatal(err)
	}
	dnsTwice, err := discovery.NewDNS("podium.svc", 7000, discovery.WithDNSResolver(dnsAnswers{{Address: "10.0.0.1:7000"}, {Address: "10.0.0.2:7000"}, {Address: "10.0.0.2:7000"}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		nodeID, advertise string
		disco             cluster.Discovery
		want              error
	}{
		{"discovery fails", self, addr, failingDiscovery{}, errDiscoveryDown},
		{"no discovery", self, addr, nil, cluster.ErrNodeNotListed},
		{"no peers", self, addr, discovery.NewStatic(nil), cluster.ErrNodeNotListed},
		{"this node missing", self, addr, others(), cluster.ErrNodeNotListed},
		{"this node at another address", self, addr, others(cluster.Peer{ID: self, Address: "10.0.0.9:7000"}), cluster.ErrNodeNotListed},
		{"its address under another ID", self, addr, others(cluster.Peer{ID: "n9", Address: addr}), cluster.ErrNodeNotListed},
		{"an ID twice", self, addr, others(cluster.Peer{ID: self, Address: addr}, cluster.Peer{ID: "n2", Address: "10.0.0.4:7000"}), cluster.ErrDuplicatePeer},
		{"an address twice", self, addr, others(cluster.Peer{ID: self, Address: addr}, cluster.Peer{ID: "n4", Address: "10.0.0.2:7000"}), cluster.ErrDuplicatePeer},
		{"a host name against DNS answers", "podium-0.podium:7000", "podium-0.podium:7000", dns, cluster.ErrNodeNotListed},
		{"an address DNS answers twice", addr, addr, dnsTwice, cluster.ErrDuplicatePeer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			servers, err := bootstrapServers(Config{NodeID: tc.nodeID}, hraft.ServerAddress(tc.advertise), tc.disco)
			if !errors.Is(err, tc.want) {
				t.Fatalf("bootstrapServers() = %v, %v; want %v", servers, err, tc.want)
			}
		})
	}
}

// A first start whose peers do not list the node fails before its transport
// listens and before the node keeps any state, so the next start, with the
// node listed, bootstraps. Another listener holds the node's port during the
// failed start: a transport that tried to listen would fail on it instead.
func TestFirstStartRefusesPeersWithoutThisNode(t *testing.T) {
	n := newLoopbackNode(t, newTestCA(t), "n1", reserveLoopbackAddrs(t, 1)[0])
	t.Cleanup(n.stop)
	holder, err := net.Listen("tcp", n.addr)
	if err != nil {
		t.Fatal(err)
	}
	err = n.open(discovery.NewStatic([]cluster.Peer{{ID: "n2", Address: "127.0.0.1:1"}}))
	_ = holder.Close()
	if !errors.Is(err, cluster.ErrNodeNotListed) {
		t.Fatalf("New() = %v, want %v", err, cluster.ErrNodeNotListed)
	}
	n.start(t, n.self())
	waitFor(t, 5*time.Second, "n1 leads its one-node cluster", n.profile.leadership.AmLeader)
}

// A first start writes its bootstrap configuration into the stores before
// hashicorp/raft runs. The transport listens already, but nothing consumes its
// RPCs before NewRaft, so no vote request can persist a term first, which
// would leave the node with state and make the bootstrap fail.
func TestFirstStartBootstrapsBeforeRaftRuns(t *testing.T) {
	var (
		term    uint64
		servers []hraft.Server
		readErr error
	)
	startRaft := newRaft
	t.Cleanup(func() { newRaft = startRaft })
	newRaft = func(conf *hraft.Config, fsm hraft.FSM, logs hraft.LogStore, stable hraft.StableStore, snaps hraft.SnapshotStore, trans hraft.Transport) (*hraft.Raft, error) {
		var entry hraft.Log
		if term, readErr = stable.GetUint64([]byte("CurrentTerm")); readErr == nil {
			if readErr = logs.GetLog(1, &entry); readErr == nil && entry.Type == hraft.LogConfiguration {
				servers = hraft.DecodeConfiguration(entry.Data).Servers
			}
		}
		return startRaft(conf, fsm, logs, stable, snaps, trans)
	}

	n := newLoopbackCluster(t, "n1").nodes[0]
	want := sortedServers([]cluster.Peer{{ID: n.id, Address: n.addr}})
	if readErr != nil || term != 1 || !slices.Equal(servers, want) {
		t.Fatalf("when raft started, the stores held term %d and servers %v (%v); want term 1 and %v", term, servers, readErr, want)
	}
}

// A restart reloads the configuration from the node's own log and does not ask
// discovery: a node that restarts while discovery fails rejoins its cluster.
func TestRestartDoesNotAskDiscovery(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	leader := c.leader(t, 5*time.Second)
	follower := c.nodes[0]
	if follower == leader {
		follower = c.nodes[1]
	}
	want := configuration(t, follower)

	follower.stop()
	follower.start(t, failingDiscovery{})
	waitFor(t, 5*time.Second, follower.id+" follows a leader again", func() bool {
		return follower.profile.leadership.CurrentLeader() != ""
	})
	if got := configuration(t, follower); !slices.Equal(got, want) {
		t.Fatalf("%s restarted with %v, want %v", follower.id, got, want)
	}
}
