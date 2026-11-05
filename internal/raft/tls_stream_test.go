package raft

import (
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
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
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
// with cfg, or plain TCP when cfg is nil. Like peerServer's, it logs nothing.
func peerTransport(t *testing.T, cfg *tls.Config) *hraft.NetworkTransport {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	trans := hraft.NewNetworkTransportWithConfig(&hraft.NetworkTransportConfig{Stream: peerLayer{Listener: l, tls: cfg}, MaxPool: 1, Timeout: 5 * time.Second, Logger: newHCLog(zerolog.Nop(), "raft-net")})
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
	trans := hraft.NewNetworkTransportWithConfig(&hraft.NetworkTransportConfig{Stream: peerLayer{Listener: l}, MaxPool: 1, Timeout: 5 * time.Second, Logger: newHCLog(zerolog.Nop(), "raft-net")})
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
	// The rogue trusts the cluster CA as well as its own, which its start checks
	// its certificate against, so only the other nodes' checks keep it out.
	rogueTLS := other.issue(t, "127.0.0.1")
	rogueTLS.CAFile = bundle(t, other, c.ca)
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

// checkRefusedStart fails the test unless New's refusal err of n shows no line
// of the key in keyFile and n's data directory was never made.
func checkRefusedStart(t *testing.T, n *loopbackNode, err error, keyFile string) {
	t.Helper()
	key, readErr := os.ReadFile(keyFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(key)), "\n") {
		if !strings.HasPrefix(line, "-----") && strings.Contains(err.Error(), line) {
			t.Fatalf("the error shows the key: %v", err)
		}
	}
	if _, err := os.Lstat(n.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the data directory of a refused node: %v, want none", err)
	}
}

// New reads the TLS files before it makes the data directory: a file that is
// missing, a key of another certificate or a CA file without a certificate
// fails the start with nothing made, and the error never shows what a file
// holds.
func TestNewRefusesUnusableTLSFiles(t *testing.T) {
	ca := newTestCA(t)
	files, other := ca.issue(t, "127.0.0.1"), ca.issue(t, "127.0.0.1")
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
			checkRefusedStart(t, n, err, files.KeyFile)
		})
	}
}

// Before its transport starts, a node checks the certificate it presents as
// its peers will: signed by the CA of ca_file, valid now, for server and for
// client authentication, which no extended key usage allows as well, and for
// the host of its advertise address as written. A certificate that fails
// stops the start with an error that says why and never shows the key, and
// nothing is made.
func TestNewChecksItsOwnCertificate(t *testing.T) {
	ca, other := newTestCA(t), newTestCA(t)
	usage := func(usages ...x509.ExtKeyUsage) func(*x509.Certificate) {
		return func(c *x509.Certificate) { c.ExtKeyUsage = usages }
	}
	strange := other.issue(t, "127.0.0.1")
	strange.CAFile = ca.file
	for _, tc := range []struct {
		name      string
		files     cluster.RaftTLSConfig
		advertise string // the host of the advertise address; empty keeps 127.0.0.1
		want      string // in the error; empty when the node starts
	}{
		{"a certificate of the CA for the advertise IP", ca.issue(t, "127.0.0.1"), "", ""},
		{"no extended key usage", ca.issueWith(t, usage(), "127.0.0.1"), "", ""},
		{"for the advertise host name", ca.issue(t, "localhost"), "localhost", ""},
		{"a certificate of another CA", strange, "", "unknown authority"},
		{"for server authentication only", ca.issueWith(t, usage(x509.ExtKeyUsageServerAuth), "127.0.0.1"), "", "client certificate"},
		{"for client authentication only", ca.issueWith(t, usage(x509.ExtKeyUsageClientAuth), "127.0.0.1"), "", "server certificate"},
		{"for another host", ca.issue(t, "10.0.0.9"), "", "advertise host"},
		{"for the IP the advertise host name resolves to", ca.issue(t, "127.0.0.1"), "localhost", "advertise host"},
		{"expired", ca.issueWith(t, func(c *x509.Certificate) {
			c.NotBefore, c.NotAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
		}, "127.0.0.1"), "", "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newLoopbackNode(t, ca, "n1", reserveLoopbackAddrs(t, 1)[0])
			t.Cleanup(n.stop)
			disco, opts := n.self(), []func(*Config){withTLS(tc.files)}
			if tc.advertise != "" {
				_, port, err := net.SplitHostPort(n.addr)
				if err != nil {
					t.Fatal(err)
				}
				advertise := net.JoinHostPort(tc.advertise, port)
				disco = discovery.NewStatic([]cluster.Peer{{ID: n.id, Address: advertise}})
				opts = append(opts, func(c *Config) { c.AdvertiseAddr = advertise })
			}
			err := n.open(disco, opts...)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("New() = %v, want the node started", err)
				}
				return
			}
			if !errors.Is(err, ErrNodeCertificate) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New() = %v, want %v saying %q", err, ErrNodeCertificate, tc.want)
			}
			checkRefusedStart(t, n, err, tc.files.KeyFile)
		})
	}
}

// A node refuses an advertise address that peers cannot dial, every interface
// or no host at all, before its transport listens, as hashicorp/raft's TCP
// transport does: errNotAdvertisable, on plain TCP and over TLS alike. Over TLS
// the node's own certificate is checked first, for the host of the advertise
// address, so a certificate that does not name that unspecified host fails the
// start earlier, as ErrNodeCertificate: that is what a caller of New that skips
// cluster.Config.Validate, which refuses such an address, gets for 0.0.0.0.
func TestNewRefusesAnAddressPeersCannotDial(t *testing.T) {
	ca := newTestCA(t)
	for _, tc := range []struct {
		name  string
		host  string // the host of the advertise address
		files cluster.RaftTLSConfig
		want  error
	}{
		{"every interface, plain TCP", "0.0.0.0", cluster.RaftTLSConfig{}, errNotAdvertisable},
		{"every IPv6 interface, plain TCP", "::", cluster.RaftTLSConfig{}, errNotAdvertisable},
		{"no host, plain TCP", "", cluster.RaftTLSConfig{}, errNotAdvertisable},
		{"every interface, a certificate that names it", "0.0.0.0", ca.issue(t, "0.0.0.0"), errNotAdvertisable},
		{"every interface, a certificate for another host", "0.0.0.0", ca.issue(t, "127.0.0.1"), ErrNodeCertificate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newLoopbackNode(t, ca, "n1", reserveLoopbackAddrs(t, 1)[0])
			t.Cleanup(n.stop)
			_, port, err := net.SplitHostPort(n.addr)
			if err != nil {
				t.Fatal(err)
			}
			advertise := net.JoinHostPort(tc.host, port)
			opts := []func(*Config){func(c *Config) { c.AdvertiseAddr = advertise }}
			if tc.files.Enabled() {
				opts = append(opts, withTLS(tc.files))
			} else {
				opts = append(opts, insecureTransport)
			}
			err = n.open(discovery.NewStatic([]cluster.Peer{{ID: n.id, Address: advertise}}), opts...)
			if !errors.Is(err, tc.want) {
				t.Fatalf("New() with the advertise address %q = %v, want %v", advertise, err, tc.want)
			}
			if l, err := net.Listen("tcp", n.addr); err != nil {
				t.Fatalf("the bind address of the refused node is taken: %v", err)
			} else {
				_ = l.Close()
			}
		})
	}
}

// partialClientHello is the start of a TLS record that announces a ClientHello
// of 124 bytes and stops after its first two, as a host that stalls halfway.
var partialClientHello = []byte{0x16, 0x03, 0x01, 0x00, 0x80, 0x01, 0x00, 0x00, 0x7c, 0x03, 0x03}

// A node closes a connection it accepted whose TLS handshake does not finish
// within handshakeTimeout: a host that connects and stays silent, or stops
// halfway through its ClientHello, holds the goroutine, the file and the buffer
// the transport gives the connection for no longer, and each such connection
// is one ERROR line of the transport. A connection whose handshake finished may
// sit idle past that bound and still carry RPCs.
func TestRaftBoundsInboundHandshakes(t *testing.T) {
	bound := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = bound })
	c := loopbackNodes(t, "n1")
	node := c.nodes[0]
	logs := node.capture()
	node.start(t, c.discovery(t))
	// dropped counts the ERROR lines of the transport about a connection it
	// gave up on.
	dropped := func() int {
		count := 0
		for _, line := range logLines(t, logs) {
			message, _ := line["message"].(string)
			if line["level"] == "error" && line["component"] == "raft-net" && strings.HasPrefix(message, "failed to decode incoming command") {
				count++
			}
		}
		return count
	}
	member := peerTransport(t, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      c.ca.pool(),
		Certificates: []tls.Certificate{certOf(t, c.ca.issue(t, "127.0.0.1"))},
	})
	if err := requestVote(member, node.addr); err != nil {
		t.Fatalf("a member's RPC: %v", err)
	}
	for _, tc := range []struct {
		name string
		send []byte
	}{
		{"a silent host", nil},
		{"half a ClientHello", partialClientHello},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := dropped()
			conn, err := net.Dial("tcp", node.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.Write(tc.send); err != nil {
				t.Fatal(err)
			}
			// This deadline only ends the wait: the node closes the connection
			// long before it.
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("the node kept the connection open for 2s, with a handshake bound of %s: %v", handshakeTimeout, err)
			}
			// The connection it closed is one ERROR line of the transport.
			waitFor(t, 5*time.Second, "the node logs the connection whose handshake timed out", func() bool {
				return dropped() == before+1
			})
		})
	}
	// The member's pooled connection has sat idle for as long as the hosts'.
	if err := requestVote(member, node.addr); err != nil {
		t.Fatalf("a member's RPC on its idle connection: %v", err)
	}
}

// A node's own address in raft, the one its leader announces, is its
// advertise address as written, as its peers list and dial it. So nodes that
// advertise a host name all know their leader by that name, the host whose
// certificate a follower checks when it forwards a write to its leader.
func TestNodesKnowTheLeaderByItsAdvertiseAddress(t *testing.T) {
	c := loopbackNodes(t, "n1", "n2", "n3")
	peers := make([]cluster.Peer, 0, len(c.nodes))
	for _, n := range c.nodes {
		_, port, err := net.SplitHostPort(n.addr)
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, cluster.Peer{ID: n.id, Address: net.JoinHostPort("localhost", port)})
	}
	for i, n := range c.nodes {
		n.start(t, discovery.NewStatic(peers), withTLS(c.ca.issue(t, "localhost")), func(cfg *Config) {
			cfg.AdvertiseAddr = peers[i].Address
		})
	}
	var leader string
	elected := c.leader(t, 5*time.Second)
	for i, n := range c.nodes {
		if n == elected {
			leader = peers[i].Address
		}
	}
	for i, n := range c.nodes {
		if got := string(n.profile.transport.LocalAddr()); got != peers[i].Address {
			t.Fatalf("%s's own address is %s, want its advertise address %s", n.id, got, peers[i].Address)
		}
		waitFor(t, 5*time.Second, n.id+" knows the leader at "+leader, func() bool {
			return n.profile.leadership.CurrentLeader() == leader
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
			n := newLoopbackNode(t, ca, "n1", reserveLoopbackAddrs(t, 1)[0])
			logs := n.capture()
			t.Cleanup(n.stop)
			err := n.open(n.self(), tc.opt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("New() = %v, want %v", err, tc.want)
			}
			out := logs.String()
			// hashicorp/raft logs through the same logger, in lines that name their component.
			own := 0
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, `"level":"warn"`) && !strings.Contains(line, `"component"`) {
					own++
				}
			}
			if own != tc.warns {
				t.Fatalf("New logged %d warnings, want %d: %s", own, tc.warns, out)
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
