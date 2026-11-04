package raft

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// loopbackTimeout is the election and heartbeat timeout of loopback nodes. It
// leaves room for a slow race-detector run, in which a shorter lease can lapse
// between a leader's election and its first write.
const loopbackTimeout = 250 * time.Millisecond

// loopbackCluster runs real nodes, each opened by New on its own loopback TCP
// port, with mutual TLS: every node presents a certificate of the cluster's CA
// for 127.0.0.1. The ports are reserved before any node starts, so every node
// lists all of them as id=127.0.0.1:port entries of cluster.discovery.peers.
type loopbackCluster struct {
	ca    *testCA
	nodes []*loopbackNode
}

// loopbackNode is one node of a loopbackCluster, or a node of its own.
type loopbackNode struct {
	id   string
	addr string                // 127.0.0.1:port, its bind and advertise address
	dir  string                // its data directory, which New creates and restarts keep
	tls  cluster.RaftTLSConfig // its certificate, key and CA, Config.TLS by default

	logger  zerolog.Logger // what New logs, nothing by default
	profile *Profile       // nil while the node is stopped
	hub     *event.Hub
}

// newLoopbackCluster starts one node per ID, each bootstrapping from
// [loopbackCluster.discovery], and stops them all when the test or benchmark
// ends. The helpers a benchmark calls take a testing.TB.
func newLoopbackCluster(t testing.TB, ids ...string) *loopbackCluster {
	t.Helper()
	c := loopbackNodes(t, ids...)
	for _, n := range c.nodes {
		n.start(t, c.discovery(t))
	}
	return c
}

// loopbackNodes is newLoopbackCluster without starting the nodes, for a test
// that changes some first.
func loopbackNodes(t testing.TB, ids ...string) *loopbackCluster {
	t.Helper()
	c := &loopbackCluster{ca: newTestCA(t)}
	for i, addr := range reserveLoopbackAddrs(t, len(ids)) {
		c.nodes = append(c.nodes, newLoopbackNode(t, c.ca, ids[i], addr))
	}
	t.Cleanup(func() {
		for _, n := range c.nodes {
			n.stop()
		}
	})
	return c
}

// newLoopbackNode returns a stopped node with a certificate of ca for
// 127.0.0.1, whose data directory does not exist yet. The caller stops it.
func newLoopbackNode(t testing.TB, ca *testCA, id, addr string) *loopbackNode {
	t.Helper()
	return &loopbackNode{
		id:     id,
		addr:   addr,
		dir:    filepath.Join(t.TempDir(), "raft"),
		tls:    ca.issue(t, "127.0.0.1"),
		logger: zerolog.Nop(),
	}
}

// discovery lists every node of c as the static driver does, from
// id=host:port entries of cluster.discovery.peers.
func (c *loopbackCluster) discovery(t testing.TB) cluster.Discovery {
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
func (c *loopbackCluster) leader(t testing.TB, timeout time.Duration) *loopbackNode {
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
		case !errors.Is(err, ErrNotLeader) && !errors.Is(err, hraft.ErrLeadershipLost):
			t.Fatalf("write: %v", err)
		case time.Now().After(deadline):
			t.Fatalf("write: %v, still after %s", err, timeout)
		}
	}
}

// start opens n from its data directory: a first start bootstraps from disco,
// a restart reloads the node's own log. The node runs mutual TLS with its own
// files; each opt edits the node's Config before New gets it.
func (n *loopbackNode) start(t testing.TB, disco cluster.Discovery, opts ...func(*Config)) {
	t.Helper()
	if err := n.open(disco, opts...); err != nil {
		t.Fatalf("start %s: %v", n.id, err)
	}
}

// open is start for a test that expects New to fail: it returns the error.
func (n *loopbackNode) open(disco cluster.Discovery, opts ...func(*Config)) error {
	cfg := Config{
		NodeID:           n.id,
		BindAddr:         n.addr,
		DataDir:          n.dir,
		ElectionTimeout:  loopbackTimeout,
		HeartbeatTimeout: loopbackTimeout,
		TLS:              n.tls,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	hub := event.NewHub(zerolog.Nop())
	p, err := New(cfg, inmemory.New(), hub, disco, n.logger)
	if err != nil {
		hub.Close()
		return err
	}
	n.profile, n.hub = p, hub
	return nil
}

// capture makes n log into a buffer, which the test reads while the node runs.
// Call it before the node starts.
func (n *loopbackNode) capture() *logBuffer {
	logs := &logBuffer{}
	n.logger = zerolog.New(logs)
	return logs
}

// logBuffer collects what a node logs. raft's goroutines log while the test
// reads it, so unlike a bytes.Buffer it is safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

// self is a discovery that lists only n, for a cluster of one node.
func (n *loopbackNode) self() cluster.Discovery {
	return discovery.NewStatic([]cluster.Peer{{ID: n.id, Address: n.addr}})
}

// state returns the state a running n holds, encoded as a snapshot encodes it,
// which orders every entity: nodes that hold the same state give the same
// bytes.
func (n *loopbackNode) state(t *testing.T) []byte {
	t.Helper()
	data, err := snapshot.Marshal(n.profile.inner.(*inmemory.Store).SnapshotForRaft())
	if err != nil {
		t.Fatalf("%s: %v", n.id, err)
	}
	return data
}

// agents returns the IDs of the agents a running n holds.
func (n *loopbackNode) agents() map[string]bool {
	ids := make(map[string]bool)
	for _, agent := range n.profile.inner.(*inmemory.Store).SnapshotForRaft().Agents {
		ids[agent.ID()] = true
	}
	return ids
}

// withTLS gives a node files other than its own.
func withTLS(files cluster.RaftTLSConfig) func(*Config) {
	return func(c *Config) { c.TLS = files }
}

// insecureTransport runs a node on plain TCP, for a test that TLS has no part
// in.
func insecureTransport(c *Config) {
	c.TLS, c.AllowInsecureTransport = cluster.RaftTLSConfig{}, true
}

// oneNodeTimeouts are short timeouts for a cluster of one node: with no peer to
// hear from, its lease cannot lapse.
func oneNodeTimeouts(c *Config) {
	c.ElectionTimeout, c.HeartbeatTimeout = 50*time.Millisecond, 50*time.Millisecond
}

// privateTestDir makes an empty data directory that only its owner can enter,
// for a test that writes into it before New opens it. t.TempDir is not
// private: New refuses it.
func privateTestDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "raft")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// reserveLoopbackAddrs returns count distinct 127.0.0.1 addresses on ports the
// kernel picked. Its listeners are closed when it returns, so nodes can bind
// the ports; another process may take one in between, a race tests accept.
func reserveLoopbackAddrs(t testing.TB, count int) []string {
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
func waitFor(t testing.TB, timeout time.Duration, what string, cond func() bool) {
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

// testCA is a certificate authority made when a test runs. Its key stays in
// memory; its certificate is written to a file of the test.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string // its certificate, PEM
}

// newTestCA makes a CA valid for the next hour.
func newTestCA(t testing.TB) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "podium raft test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ca := &testCA{cert: cert, key: key, file: filepath.Join(t.TempDir(), "ca.crt")}
	writePEM(t, ca.file, "CERTIFICATE", der)
	return ca
}

// issue writes a certificate of ca for hosts, each an IP or a DNS subject
// alternative name, usable as a server and as a client certificate, and its
// key, and returns the files of a node that presents it and trusts ca.
func (ca *testCA) issue(t testing.TB, hosts ...string) cluster.RaftTLSConfig {
	t.Helper()
	return ca.issueWith(t, func(*x509.Certificate) {}, hosts...)
}

// issueWith is issue with edit applied to the certificate before ca signs it.
func (ca *testCA) issueWith(t testing.TB, edit func(*x509.Certificate), hosts ...string) cluster.RaftTLSConfig {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	edit(tmpl)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := cluster.RaftTLSConfig{CertFile: filepath.Join(dir, "node.crt"), KeyFile: filepath.Join(dir, "node.key"), CAFile: ca.file}
	writePEM(t, files.CertFile, "CERTIFICATE", der)
	writePEM(t, files.KeyFile, "PRIVATE KEY", keyDER)
	return files
}

// pool returns a pool of ca's certificate alone.
func (ca *testCA) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// bundle writes the certificates of cas to one CA file and returns it.
func bundle(t *testing.T, cas ...*testCA) string {
	t.Helper()
	var pemBytes []byte
	for _, ca := range cas {
		pemBytes = append(pemBytes, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})...)
	}
	file := filepath.Join(t.TempDir(), "bundle.crt")
	if err := os.WriteFile(file, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// writePEM writes der to path as one PEM block of typ, readable by its owner
// only.
func writePEM(t testing.TB, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
