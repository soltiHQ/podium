package raft

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/fs"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// peerLayer is the stream layer of a peer a test plays: it dials with its TLS
// configuration, or plain TCP without one, and accepts on its listener.
type peerLayer struct {
	net.Listener
	tls *tls.Config
}

func (l peerLayer) Dial(address hraft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if l.tls == nil {
		return dialer.Dial("tcp", string(address))
	}
	return tls.DialWithDialer(dialer, "tcp", string(address), l.tls)
}

// peerTransport returns the raft transport of a peer a test plays, which dials
// with cfg, or plain TCP when cfg is nil.
func peerTransport(t *testing.T, cfg *tls.Config) *hraft.NetworkTransport {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	trans := hraft.NewNetworkTransportWithConfig(&hraft.NetworkTransportConfig{Stream: peerLayer{Listener: l, tls: cfg}, MaxPool: 1, Timeout: 5 * time.Second})
	t.Cleanup(func() { _ = trans.Close() })
	return trans
}

// peerServer starts the raft transport of a peer a test plays on a loopback
// port, with mutual TLS on cfg or plain TCP when cfg is nil, answers every RPC
// it gets, and returns its address.
func peerServer(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		l = tls.NewListener(l, cfg)
	}
	trans := hraft.NewNetworkTransportWithConfig(&hraft.NetworkTransportConfig{Stream: peerLayer{Listener: l}, MaxPool: 1, Timeout: 5 * time.Second})
	done := make(chan struct{})
	go func() {
		for {
			select {
			case rpc := <-trans.Consumer():
				rpc.Respond(&hraft.RequestVoteResponse{}, nil)
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(done)
		_ = trans.Close()
	})
	return l.Addr().String()
}

// requestVote sends a vote request through trans to addr from a candidate in
// no configuration, at term 0, which a node only answers.
func requestVote(trans hraft.Transport, addr string) error {
	var resp hraft.RequestVoteResponse
	return trans.RequestVote("intruder", hraft.ServerAddress(addr), &hraft.RequestVoteRequest{
		RPCHeader: hraft.RPCHeader{ProtocolVersion: hraft.ProtocolVersionMax, ID: []byte("intruder"), Addr: []byte("intruder")},
	}, &resp)
}

// certOf loads the certificate and key of files.
func certOf(t *testing.T, files cluster.RaftTLSConfig) tls.Certificate {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// A peer gets an RPC through to a node, and a node gets one through to a peer,
// only with mutual TLS 1.3 on certificates of the cluster CA. So a node whose
// certificate another CA signed, even one that trusts the cluster CA, cannot
// join: the other two commit a write that it never gets, and it never learns
// their leader. A node answers no RPC from a peer with a certificate of another
// CA, with none, over TLS 1.2 or over plain TCP, and gets no RPC through to a
// peer with a certificate of another CA, over TLS 1.2 or over plain TCP.
func TestRaftRefusesPeerWithoutClusterCert(t *testing.T) {
	c := loopbackNodes(t, "n1", "n2", "n3")
	members, rogue := c.nodes[:2], c.nodes[2]
	other := newTestCA(t)
	// The rogue trusts the cluster CA, so only the other nodes' checks keep it out.
	rogueTLS := other.issue(t, "127.0.0.1")
	rogueTLS.CAFile = c.ca.file
	for _, n := range members {
		n.start(t, c.discovery(t))
	}
	rogue.start(t, c.discovery(t), withTLS(rogueTLS))

	ctx := context.Background()
	agent, err := model.NewAgent("kept-from-the-rogue", "replicated", "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	c.write(t, 5*time.Second, func(s storage.Storage) error { return s.UpsertAgent(ctx, agent) })
	for _, n := range members {
		waitFor(t, 5*time.Second, n.id+" applies the write", func() bool {
			_, err := n.profile.Store().GetAgent(ctx, agent.ID())
			return err == nil
		})
	}
	// The leader of n1 and n2 keeps failing to heartbeat the rogue after the
	// write, so the rogue cannot have got it.
	failed := make(chan hraft.Observation, 1)
	for _, n := range members {
		observer := hraft.NewObserver(failed, false, func(o *hraft.Observation) bool {
			heartbeat, ok := o.Data.(hraft.FailedHeartbeatObservation)
			return ok && heartbeat.PeerID == hraft.ServerID(rogue.id)
		})
		n.profile.raft.RegisterObserver(observer)
		defer n.profile.raft.DeregisterObserver(observer)
	}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatalf("no failed heartbeat to %s within 5s", rogue.id)
	}
	if leader := rogue.profile.leadership.CurrentLeader(); leader != "" {
		t.Fatalf("%s follows the leader at %s", rogue.id, leader)
	}
	if _, err := rogue.profile.Store().GetAgent(ctx, agent.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("%s reads the write: %v, want %v", rogue.id, err, storage.ErrNotFound)
	}

	var (
		pool     = c.ca.pool()
		member   = certOf(t, c.ca.issue(t, "127.0.0.1"))
		stranger = certOf(t, other.issue(t, "127.0.0.1"))
		node     = c.nodes[0]
	)
	for _, tc := range []struct {
		name string
		tls  *tls.Config // the peer's client side; nil is plain TCP
		ok   bool
	}{
		{"a certificate of the cluster CA", &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{member}}, true},
		{"a certificate of another CA", &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{stranger}}, false},
		{"no certificate", &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}, false},
		{"TLS 1.2", &tls.Config{MaxVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{member}}, false},
		{"plain TCP", nil, false},
	} {
		t.Run("from a peer with "+tc.name, func(t *testing.T) {
			err := requestVote(peerTransport(t, tc.tls), node.addr)
			if tc.ok && err != nil {
				t.Fatalf("%s did not answer: %v", node.id, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("%s answered", node.id)
			}
		})
	}

	// server is the server side of a peer that presents cert and wants a client
	// certificate of the cluster CA, so only the node's own checks can fail.
	server := func(cert tls.Certificate, maxVersion uint16) *tls.Config {
		return &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MaxVersion: maxVersion}
	}
	var unknownCA x509.UnknownAuthorityError
	for _, tc := range []struct {
		name string
		tls  *tls.Config // the peer's server side; nil is plain TCP
		want func(error) bool
	}{
		{"a certificate of the cluster CA", server(member, 0), func(err error) bool { return err == nil }},
		{"a certificate of another CA", server(stranger, 0), func(err error) bool { return errors.As(err, &unknownCA) }},
		{"TLS 1.2", server(member, tls.VersionTLS12), func(err error) bool { return err != nil }},
		{"plain TCP", nil, func(err error) bool { return err != nil }},
	} {
		t.Run("to a peer with "+tc.name, func(t *testing.T) {
			if err := requestVote(node.profile.transport, peerServer(t, tc.tls)); !tc.want(err) {
				t.Fatalf("%s's RPC: %v", node.id, err)
			}
		})
	}
}

// A node dials a peer only when the peer's certificate of the cluster CA names
// the host it dials, which is the host of the peer's address as the
// configuration lists it: compared as written, as an IP or a DNS name, not as
// the name resolves.
func TestRaftRefusesPeerCertForAnotherHost(t *testing.T) {
	c := newLoopbackCluster(t, "n1")
	node := c.nodes[0]
	pool := c.ca.pool()
	for _, tc := range []struct {
		name  string
		hosts []string // the subject alternative names of the peer's certificate
		dial  string   // the host the node dials the peer at
		ok    bool
	}{
		{"the dialed IP", []string{"127.0.0.1"}, "127.0.0.1", true},
		{"the dialed host name", []string{"localhost"}, "localhost", true},
		{"another IP", []string{"10.0.0.9"}, "127.0.0.1", false},
		{"another host name", []string{"podium-9.podium"}, "127.0.0.1", false},
		{"the IP the dialed host name resolves to", []string{"127.0.0.1"}, "localhost", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := peerServer(t, &tls.Config{
				Certificates: []tls.Certificate{certOf(t, c.ca.issue(t, tc.hosts...))},
				ClientCAs:    pool,
				ClientAuth:   tls.RequireAndVerifyClientCert,
			})
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatal(err)
			}
			err = requestVote(node.profile.transport, net.JoinHostPort(tc.dial, port))
			var hostname x509.HostnameError
			switch {
			case tc.ok && err != nil:
				t.Fatalf("RPC to a peer with a certificate for %v, dialed at %s: %v", tc.hosts, tc.dial, err)
			case !tc.ok && !errors.As(err, &hostname):
				t.Fatalf("RPC to a peer with a certificate for %v, dialed at %s: %v, want %T", tc.hosts, tc.dial, err, hostname)
			}
		})
	}
}

// New reads the TLS files before it makes the data directory: a file that is
// missing, a key of another certificate or a CA file without a certificate
// fails the start with nothing made, and the error never shows what a file
// holds.
func TestNewRefusesUnusableTLSFiles(t *testing.T) {
	ca := newTestCA(t)
	files, other := ca.issue(t, "127.0.0.1"), ca.issue(t, "127.0.0.1")
	key, err := os.ReadFile(files.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*cluster.RaftTLSConfig)
	}{
		{"a missing cert_file", func(f *cluster.RaftTLSConfig) { f.CertFile += ".missing" }},
		{"a key of another certificate", func(f *cluster.RaftTLSConfig) { f.KeyFile = other.KeyFile }},
		{"a key as cert_file", func(f *cluster.RaftTLSConfig) { f.CertFile = f.KeyFile }},
		{"a key as ca_file", func(f *cluster.RaftTLSConfig) { f.CAFile = f.KeyFile }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newLoopbackNode(t, ca, "n1", reserveLoopbackAddrs(t, 1)[0])
			t.Cleanup(n.stop)
			unusable := files
			tc.edit(&unusable)
			err := n.open(n.self(), withTLS(unusable))
			if err == nil {
				t.Fatal("New accepted the files")
			}
			for _, line := range strings.Split(strings.TrimSpace(string(key)), "\n") {
				if !strings.HasPrefix(line, "-----") && strings.Contains(err.Error(), line) {
					t.Fatalf("the error shows the key: %v", err)
				}
			}
			if _, err := os.Lstat(n.dir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the data directory of a refused node: %v, want none", err)
			}
		})
	}
}

// The transport runs plain TCP only with AllowInsecureTransport, and New then
// logs one WARN that names it; with neither TLS nor the flag, New refuses the
// node before it makes its data directory. A node on mutual TLS, the flag set
// or not, logs no such warning and answers no plain TCP RPC.
func TestPlainTCPNeedsAllowInsecureTransport(t *testing.T) {
	ca := newTestCA(t)
	for _, tc := range []struct {
		name  string
		opt   func(*Config)
		want  error // of New
		plain bool  // whether the node answers a plain TCP RPC
		warns int
	}{
		{"mutual TLS", func(*Config) {}, nil, false, 0},
		{"mutual TLS with allow_insecure_transport", func(c *Config) { c.AllowInsecureTransport = true }, nil, false, 0},
		{"allow_insecure_transport", insecureTransport, nil, true, 1},
		{"neither", func(c *Config) { c.TLS = cluster.RaftTLSConfig{} }, cluster.ErrRaftTLSRequired, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			n := newLoopbackNode(t, ca, "n1", reserveLoopbackAddrs(t, 1)[0])
			n.logger = zerolog.New(&logs)
			t.Cleanup(n.stop)
			err := n.open(n.self(), tc.opt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("New() = %v, want %v", err, tc.want)
			}
			out := logs.String()
			if warns := strings.Count(out, `"level":"warn"`); warns != tc.warns {
				t.Fatalf("New logged %d warnings, want %d: %s", warns, tc.warns, out)
			}
			if tc.warns > 0 && !strings.Contains(out, "allow_insecure_transport") {
				t.Fatalf("the warning does not name allow_insecure_transport: %s", out)
			}
			if err != nil {
				if _, err := os.Lstat(n.dir); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("the data directory of a refused node: %v, want none", err)
				}
				return
			}
			if answered := requestVote(peerTransport(t, nil), n.addr) == nil; answered != tc.plain {
				t.Fatalf("the node answered a plain TCP RPC: %v, want %v", answered, tc.plain)
			}
		})
	}
}
