package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/tlsconf"
)

// raftTimeoutMs is the election and heartbeat timeout of the nodes raftCluster
// starts. It leaves room for a slow race-detector run, as in the raft package.
const raftTimeoutMs = 250

// raftNode is one node of a raftCluster, as buildCluster returns it.
type raftNode struct {
	id, addr   string // addr is 127.0.0.1:port, its bind and advertise address
	store      storage.Storage
	leadership cluster.Leadership
	hub        *event.Hub
	admin      cluster.Admin
	stop       func() // shuts the node down, once; the end of the test does too
}

// raftCluster starts one raft node per ID through buildCluster, as New does, with
// the configurations raftConfigs gives them. It returns the leader, once it is
// ready and every node knows it, with the other nodes, and stops them all when
// the test ends.
func raftCluster(t *testing.T, ids ...string) (leader *raftNode, followers []*raftNode) {
	t.Helper()
	nodes := make([]*raftNode, 0, len(ids))
	for _, cfg := range raftConfigs(t, ids...) {
		nodes = append(nodes, startRaftNode(t, cfg))
	}
	return readyLeader(t, nodes)
}

// raftConfigs returns the configuration of one raft node per ID, on loopback
// ports with mutual TLS: each presents a certificate of one CA for 127.0.0.1,
// and for localhost, which a test can dial a node at as well, keeps its state in
// a data directory of its own, and lists all of them as id=127.0.0.1:port peers.
// Nothing starts: a test can start each node when it wants, through
// startRaftNode or New.
func raftConfigs(t *testing.T, ids ...string) []cluster.Config {
	t.Helper()
	ca := newTestCA(t)
	addrs := loopbackAddrs(t, len(ids))
	peers := make([]string, 0, len(ids))
	for i, id := range ids {
		peers = append(peers, id+"="+addrs[i])
	}
	cfgs := make([]cluster.Config, 0, len(ids))
	for i, id := range ids {
		cert, key := ca.issue(t, "127.0.0.1", "localhost")
		cfg := cluster.DefaultConfig()
		cfg.Backend = cluster.BackendRaft
		cfg.Raft = cluster.RaftConfig{
			NodeID: id, BindAddr: addrs[i], DataDir: filepath.Join(t.TempDir(), "raft"),
			ElectionTimeoutMs: raftTimeoutMs, HeartbeatTimeoutMs: raftTimeoutMs,
			TLS: cluster.RaftTLSConfig{CertFile: cert, KeyFile: key, CAFile: ca.file},
		}
		cfg.Discovery.Peers = peers
		cfgs = append(cfgs, cfg)
	}
	return cfgs
}

// startRaftNode starts the raft node of cfg through buildCluster, as New does,
// and stops it when the test ends, if stop did not first.
func startRaftNode(t *testing.T, cfg cluster.Config) *raftNode {
	t.Helper()
	return startRaftNodeLogging(t, cfg, zerolog.Nop())
}

// startRaftNodeLogging is startRaftNode with the node logging into logger.
func startRaftNodeLogging(t *testing.T, cfg cluster.Config, logger zerolog.Logger) *raftNode {
	t.Helper()
	store, leadership, hub, shutdown, admin, err := buildCluster(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	n := &raftNode{id: cfg.Raft.NodeID, addr: cfg.Raft.BindAddr, store: store, leadership: leadership, hub: hub, admin: admin}
	n.stop = sync.OnceFunc(func() {
		hub.Close()
		shutdown()
	})
	t.Cleanup(n.stop)
	return n
}

// readyLeader waits until one of nodes leads, is ready and is known by every
// other node, and returns it with the other nodes. A stopped node must not be
// among nodes: it knows no leader.
func readyLeader(t *testing.T, nodes []*raftNode) (leader *raftNode, followers []*raftNode) {
	t.Helper()
	waitFor(t, 10*time.Second, "one ready leader that every node knows", func() bool {
		leader, followers = nil, nil
		for _, n := range nodes {
			switch {
			case n.leadership.AmLeader() && leader != nil:
				return false
			case n.leadership.AmLeader():
				leader = n
			default:
				followers = append(followers, n)
			}
		}
		if leader == nil {
			return false
		}
		for _, n := range followers {
			if n.leadership.CurrentLeader() != leader.addr {
				return false
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return cluster.AwaitLeaderReady(ctx, leader.leadership) == nil
	})
	return leader, followers
}

// waitFor polls cond every 10ms until it holds, and fails the test when it still
// does not after timeout.
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

// nodeDiscovery is the discovery of a raft node, wired from cfg as New wires it:
// both listeners over the node's store, with the client IP resolver of the
// node's raft configuration and, on a follower, the forwarder of its gRPC syncs.
type nodeDiscovery struct {
	http   http.Handler
	grpc   *grpc.Server
	agents *agent.Service
}

func newNodeDiscovery(t *testing.T, cfg config.Config, n *raftNode) nodeDiscovery {
	t.Helper()
	logger := zerolog.Nop()
	serverTLS, err := cfg.TLS.Server.Build()
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := cfg.TLS.Client.Build()
	if err != nil {
		t.Fatal(err)
	}
	resolver, closeHosts, err := clientResolver(cfg, n.leadership)
	if err != nil {
		t.Fatal(err)
	}
	if closeHosts != nil {
		t.Cleanup(closeHosts)
	}
	forwarder := syncForwarder(cfg, n.leadership, clientTLS)
	t.Cleanup(func() { _ = forwarder.Close() })
	agents := agent.New(n.store, logger)
	h, srv := buildDiscoveryListeners(logger, agents, n.hub, n.leadership, cfg, resolver, serverTLS, clientTLS, forwarder)
	// Before the forwarder closes: cleanups run last in, first out.
	t.Cleanup(srv.Stop)
	return nodeDiscovery{http: h, grpc: srv, agents: agents}
}

// listenLoopback listens on a loopback port the kernel picks.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	return lis
}

// serveGRPC serves srv on lis and returns the address to dial it at.
func serveGRPC(srv *grpc.Server, lis net.Listener) string {
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String()
}

// fromIP is a listener whose connections come from ip, whatever their real peer:
// a client at an address the test picks, for the server that serves on it.
type fromIP struct {
	net.Listener
	ip string
}

func (l fromIP) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return peerConn{Conn: conn, peer: &net.TCPAddr{IP: net.ParseIP(l.ip), Port: 40000}}, nil
}

// peerConn is a connection from peer.
type peerConn struct {
	net.Conn
	peer net.Addr
}

func (c peerConn) RemoteAddr() net.Addr { return c.peer }

// discoveryClient dials the gRPC discovery at addr, over TLS when roots is set:
// the CA that verifies the server, with clientCert presented when set too.
func discoveryClient(t *testing.T, addr string, roots *testCA, clientCert *tls.Certificate) discoverv1.DiscoverServiceClient {
	t.Helper()
	creds := insecure.NewCredentials()
	if roots != nil {
		pool := x509.NewCertPool()
		pool.AddCert(roots.cert)
		cfg := &tls.Config{RootCAs: pool}
		if clientCert != nil {
			cfg.Certificates = []tls.Certificate{*clientCert}
		}
		creds = credentials.NewTLS(cfg)
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return discoverv1.NewDiscoverServiceClient(conn)
}

// syncRequest is the heartbeat of the agent id.
func syncRequest(id string) *discoverv1.SyncRequest {
	return &discoverv1.SyncRequest{Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1}
}

// withToken is ctx with token as the bearer token of a call.
func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// A follower's gRPC discovery forwards a Sync to the leader's gRPC listener, at
// the host of the leader's raft address and the follower's own gRPC port, over
// TLS with tls.client when tls.server is set, presenting tls.client's
// certificate when the leader wants one, as HTTP forwarding does. The agent's
// sync lands in the leader's store, which replicates it to every node, and the
// follower answers with the leader's answer. The token goes along unchanged:
// the leader enrolls the very token the agent sent, and refuses another one.
func TestFollowerForwardsGRPCSync(t *testing.T) {
	leader, followers := raftCluster(t, "n1", "n2", "n3")
	follower := followers[0]
	ca := newTestCA(t)
	// One certificate for every listener and for tls.client: it names the raft
	// host 127.0.0.1, which the follower dials the leader at.
	certFile, keyFile := ca.issue(t, "127.0.0.1")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		clientCA bool // the listeners want a client certificate of ca
	}{
		{"tls", false},
		{"mutual tls", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.TLS.Server = tlsconf.ServerConfig{CertFile: certFile, KeyFile: keyFile}
			cfg.TLS.Client = tlsconf.ClientConfig{CAFile: ca.file}
			var agentCert *tls.Certificate
			if tc.clientCA {
				cfg.TLS.Server.ClientCAFile = ca.file
				cfg.TLS.Client.CertFile, cfg.TLS.Client.KeyFile = certFile, keyFile
				agentCert = &cert
			}
			leaderLis := listenLoopback(t)
			// The leader serves gRPC on this port, and so is every node assumed to.
			cfg.GRPC.Addr = leaderLis.Addr().String()
			leaderDiscovery := newNodeDiscovery(t, cfg, leader)
			serveGRPC(leaderDiscovery.grpc, leaderLis)
			// The follower serves on a port of its own, which it does not forward to.
			client := discoveryClient(t, serveGRPC(newNodeDiscovery(t, cfg, follower).grpc, listenLoopback(t)), ca, agentCert)

			ctx := context.Background()
			id, token := "agent-"+strings.ReplaceAll(tc.name, " ", "-"), "token-of-the-agent"
			resp, err := client.Sync(withToken(ctx, token), syncRequest(id))
			if err != nil || !resp.GetSuccess() {
				t.Fatalf("sync through the follower: %v %v, want the leader's success", resp, err)
			}
			if stored, err := leaderDiscovery.agents.AgentToken(ctx, id); err != nil || stored != token {
				t.Fatalf("the leader enrolled %q (%v), want the token the agent sent", stored, err)
			}
			for _, n := range append([]*raftNode{leader}, followers...) {
				waitFor(t, 5*time.Second, n.id+" holds the agent", func() bool {
					_, err := n.store.GetAgent(ctx, id)
					return err == nil
				})
			}
			if _, err := client.Sync(withToken(ctx, "another-token"), syncRequest(id)); status.Code(err) != codes.Unauthenticated {
				t.Fatalf("sync with another token through the follower: %v, want the leader's Unauthenticated", err)
			}
		})
	}
}

// syncOutcome is how a discovery sync ended, the same over either transport.
type syncOutcome string

const (
	synced          syncOutcome = "synced"
	unauthenticated syncOutcome = "unauthenticated"
	limited         syncOutcome = "limited"
)

func grpcOutcome(err error) syncOutcome {
	switch status.Code(err) {
	case codes.OK:
		return synced
	case codes.Unauthenticated:
		return unauthenticated
	case codes.ResourceExhausted:
		return limited
	}
	return syncOutcome(err.Error())
}

func httpOutcome(w *httptest.ResponseRecorder) syncOutcome {
	switch w.Code {
	case http.StatusOK:
		return synced
	case http.StatusUnauthorized:
		return unauthenticated
	case http.StatusTooManyRequests:
		return limited
	}
	return syncOutcome(fmt.Sprintf("HTTP %d %s", w.Code, w.Body.String()))
}

// httpSyncFrom posts the heartbeat of the agent id with token to the HTTP
// discovery h, from the peer ip, which names forwardedFor in X-Forwarded-For
// when set.
func httpSyncFrom(h http.Handler, ip, forwardedFor, id, token string) *httptest.ResponseRecorder {
	body := `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
	r.RemoteAddr = net.JoinHostPort(ip, "40000")
	r.Header.Set("Authorization", "Bearer "+token)
	if forwardedFor != "" {
		r.Header.Set("X-Forwarded-For", forwardedFor)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A follower forwards a sync with the agent's address, in x-forwarded-for over
// gRPC and in X-Forwarded-For over HTTP, and the leader believes it from a host
// of its raft configuration: its limiter counts the agent's token failures at
// the agent's own address, not at the follower's. Every attempt also names an
// address of its own in that header, which counts for nothing: the follower
// appends the agent's address, where the leader's walk stops, as the agent is no
// trusted host. After five failures from one address the agent is refused
// there, its sixth attempt naming yet another address included, and still gets
// in through the same follower from another address.
func TestForwardedSyncKeepsClientIPForLimiter(t *testing.T) {
	leader, followers := raftCluster(t, "n1", "n2", "n3")
	follower := followers[0]
	// Two addresses of the agent, as the follower sees them; neither is a host of
	// the raft configuration.
	const blocked, other = "203.0.113.9", "198.51.100.7"
	for _, tc := range []struct {
		name string
		// through returns a sync through the follower from an address of the
		// agent, which names forged in its forwarding header.
		through func(t *testing.T) func(from, forged, id, token string) syncOutcome
	}{
		{"grpc", func(t *testing.T) func(string, string, string, string) syncOutcome {
			cfg := config.Default()
			leaderLis := listenLoopback(t)
			cfg.GRPC.Addr = leaderLis.Addr().String()
			serveGRPC(newNodeDiscovery(t, cfg, leader).grpc, leaderLis)
			srv := newNodeDiscovery(t, cfg, follower).grpc
			clients := map[string]discoverv1.DiscoverServiceClient{}
			for _, from := range []string{blocked, other} {
				clients[from] = discoveryClient(t, serveGRPC(srv, fromIP{Listener: listenLoopback(t), ip: from}), nil, nil)
			}
			return func(from, forged, id, token string) syncOutcome {
				ctx := metadata.AppendToOutgoingContext(withToken(context.Background(), token), "x-forwarded-for", forged)
				_, err := clients[from].Sync(ctx, syncRequest(id))
				return grpcOutcome(err)
			}
		}},
		{"http", func(t *testing.T) func(string, string, string, string) syncOutcome {
			cfg := config.Default()
			leaderSrv := httptest.NewServer(newNodeDiscovery(t, cfg, leader).http)
			t.Cleanup(leaderSrv.Close)
			_, port, err := net.SplitHostPort(leaderSrv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			cfg.HTTPDiscovery.Addr = ":" + port
			h := newNodeDiscovery(t, cfg, follower).http
			return func(from, forged, id, token string) syncOutcome {
				return httpOutcome(httpSyncFrom(h, from, forged, id, token))
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			through := tc.through(t)
			id := "agent-" + tc.name
			attempt := 0
			sync := func(from, token string) syncOutcome {
				attempt++
				return through(from, fmt.Sprintf("192.0.2.%d", attempt), id, token)
			}
			if got := sync(blocked, "token-"+id); got != synced {
				t.Fatalf("enroll through the follower: %s, want %s", got, synced)
			}
			for i := range 5 {
				if got := sync(blocked, "stale-token"); got != unauthenticated {
					t.Fatalf("bad token %d through the follower: %s, want %s", i+1, got, unauthenticated)
				}
			}
			if got := sync(blocked, "token-"+id); got != limited {
				t.Fatalf("the agent after five failures from %s, each naming another address: %s, want %s: a forged header counted", blocked, got, limited)
			}
			if got := sync(other, "token-"+id); got != synced {
				t.Fatalf("the agent from %s through the same follower: %s, want %s: the leader counted the failures at the follower's address", other, got, synced)
			}
		})
	}
}

// A follower refuses a cross-origin browser mutation itself, with the 403 of the
// cross-origin check, and logs it, before its Leader middleware forwards
// anything: the leader never sees the request. A same-origin mutation is still
// forwarded.
func TestCrossOriginRefusedOnFollower(t *testing.T) {
	reached := make(chan string, 8)
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(leader.Close)
	_, port, err := net.SplitHostPort(leader.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HTTP.Addr = ":" + port
	logs := &syncBuffer{}
	h := mainHandlerOf(t, cfg, inmemory.New(), zerolog.New(logs), follower{leader: "127.0.0.1:7000"}, nil)

	for _, tc := range []struct {
		name, target, contentType, body string
		site                            string // Sec-Fetch-Site; the Origin is browserOrigin for same-origin, another site otherwise
		forwarded                       bool
	}{
		{"cross-site API mutation", "/api/v1/users", jsonType, `{"subject":"mallory"}`, "cross-site", false},
		{"cross-site login form", "/login", formType, loginForm("alice"), "cross-site", false},
		{"same-origin API mutation", "/api/v1/users", jsonType, `{"subject":"carol"}`, "same-origin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := browserRequest(http.MethodPost, tc.target, tc.contentType, tc.body, nil)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			if !tc.forwarded {
				r.Header.Set("Origin", "https://other.example.com")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if tc.forwarded {
				if w.Code != http.StatusAccepted {
					t.Fatalf("follower answered %d %q, want the leader's %d", w.Code, w.Body.String(), http.StatusAccepted)
				}
				if got := <-reached; got != http.MethodPost+" "+tc.target {
					t.Fatalf("the leader got %s, want POST %s", got, tc.target)
				}
				return
			}
			if w.Code != http.StatusForbidden {
				t.Fatalf("follower answered %d %q, want 403", w.Code, w.Body.String())
			}
			select {
			case got := <-reached:
				t.Fatalf("the leader got %s, a request the follower must refuse", got)
			default:
			}
		})
	}
	refusals := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry struct{ Level, Message string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Level == "warn" && entry.Message == "cross-origin request blocked" {
			refusals++
		}
	}
	if refusals != 2 {
		t.Fatalf("the follower logged %d refusals at WARN, want 2:\n%s", refusals, logs.String())
	}
}

// A follower's CORS middleware answers a cross-origin client for the write it
// forwards, and the leader's own CORS headers are dropped from the forwarded
// answer: it carries Access-Control-Allow-Origin once, and Origin once in Vary,
// as a browser needs to accept it.
func TestForwardedWriteCarriesTheCORSHeadersOnce(t *testing.T) {
	const origin = "https://app.example.com"
	cfg := config.Default()
	cfg.CORS.AllowOrigins = []string{origin}
	cfg.CORS.AllowCredentials = true
	leader := httptest.NewServer(mainHandlerOf(t, cfg, inmemory.New(), zerolog.Nop(), standalone.NewLeadership(), nil))
	t.Cleanup(leader.Close)
	_, port, err := net.SplitHostPort(leader.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTP.Addr = ":" + port
	h := mainHandlerOf(t, cfg, inmemory.New(), zerolog.Nop(), follower{leader: "127.0.0.1:7000"}, nil)

	r := browserRequest(http.MethodPost, "/api/v1/users", jsonType, `{"subject":"carol"}`, nil)
	r.Header.Set("Origin", origin)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	// The leader refuses the write, which carries no credentials: an answer of the
	// leader all the same, with the request ID that only its routes set.
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Request-Id") == "" {
		t.Fatalf("follower answered %d with request ID %q, want the leader's 401", w.Code, w.Header().Get("X-Request-Id"))
	}
	for _, name := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Expose-Headers"} {
		if got := w.Header().Values(name); len(got) != 1 {
			t.Errorf("%s %q, want one value", name, got)
		}
	}
	origins := 0
	for _, line := range w.Header().Values("Vary") {
		for _, token := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "Origin") {
				origins++
			}
		}
	}
	if origins != 1 {
		t.Errorf("Vary %q names Origin %d times, want once", w.Header().Values("Vary"), origins)
	}
}

// A host of the raft configuration written as a name is trusted at the addresses
// the name resolves to, looked up through the system resolver: once a node that
// advertises localhost has looked its name up, the forwarding headers of
// 127.0.0.1 count, and still no one else's.
func TestRaftHostNamesAreTrustedAtTheirAddresses(t *testing.T) {
	ca := newTestCA(t)
	raftCert, raftKey := ca.issue(t, "localhost")
	bind := loopbackAddrs(t, 1)[0]
	_, raftPort, err := net.SplitHostPort(bind)
	if err != nil {
		t.Fatal(err)
	}
	advertise := net.JoinHostPort("localhost", raftPort)
	raftCfg := cluster.DefaultConfig()
	raftCfg.Backend = cluster.BackendRaft
	raftCfg.Raft = cluster.RaftConfig{
		NodeID: "n1", BindAddr: bind, AdvertiseAddr: advertise, DataDir: filepath.Join(t.TempDir(), "raft"),
		ElectionTimeoutMs: 50, HeartbeatTimeoutMs: 50,
		TLS: cluster.RaftTLSConfig{CertFile: raftCert, KeyFile: raftKey, CAFile: ca.file},
	}
	raftCfg.Discovery.Peers = []string{"n1=" + advertise}
	_, leadership, hub, shutdown, _, err := buildCluster(raftCfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		hub.Close()
		shutdown()
	})
	resolver, closeHosts, err := clientResolver(config.Default(), leadership)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeHosts)

	client := func(peer string) string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = net.JoinHostPort(peer, "40000")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		return resolver.IP(r)
	}
	waitFor(t, 5*time.Second, "the forwarding headers of 127.0.0.1, where localhost resolves, count", func() bool {
		return client("127.0.0.1") == "203.0.113.9"
	})
	if got := client("192.0.2.50"); got != "192.0.2.50" {
		t.Fatalf("a client on another host: IP = %q, want its own address", got)
	}
}

// In raft mode the leader believes the forwarding headers of the hosts of its
// raft configuration and of trusted_proxies, no one else's: a client on another
// host that names other addresses in X-Forwarded-For, or in x-forwarded-for over
// gRPC, keeps its own address as its client IP. Every attempt here names an
// address of its own, and the limiter still counts all of them at the client's:
// the sixth is refused, on both discovery listeners and at login.
func TestUntrustedForwardedHeaderIgnored(t *testing.T) {
	leader, _ := raftCluster(t, "n1") // its configuration lists the host 127.0.0.1
	const client = "192.0.2.50"       // neither a raft host nor a trusted proxy
	forged := func(attempt int) string { return fmt.Sprintf("198.51.100.%d", attempt+1) }
	cfg := config.Default()
	cfg.TrustedProxies = []string{"10.0.0.0/8"}

	for _, tc := range []struct {
		name string
		// attempt returns how attempt ended, with a forged address of its own: a
		// sync of an agent with token, or a login with a wrong password.
		attempt func(t *testing.T) func(attempt int, token string) syncOutcome
		enroll  bool // the first attempt enrolls the agent with its token
	}{
		{"grpc discovery", func(t *testing.T) func(int, string) syncOutcome {
			discovery := discoveryClient(t, serveGRPC(newNodeDiscovery(t, cfg, leader).grpc, fromIP{Listener: listenLoopback(t), ip: client}), nil, nil)
			return func(attempt int, token string) syncOutcome {
				ctx := metadata.AppendToOutgoingContext(withToken(context.Background(), token), "x-forwarded-for", forged(attempt))
				_, err := discovery.Sync(ctx, syncRequest("agent-grpc"))
				return grpcOutcome(err)
			}
		}, true},
		{"http discovery", func(t *testing.T) func(int, string) syncOutcome {
			h := newNodeDiscovery(t, cfg, leader).http
			return func(attempt int, token string) syncOutcome {
				return httpOutcome(httpSyncFrom(h, client, forged(attempt), "agent-http", token))
			}
		}, true},
		{"login", func(t *testing.T) func(int, string) syncOutcome {
			// A user of its own per attempt, so that only the client IP key counts,
			// each with a cheap hash: an unknown user costs a compare at the full cost.
			ctx := context.Background()
			creds := credential.New(leader.store, zerolog.Nop())
			for i := range 6 {
				user, err := model.NewUser(fmt.Sprintf("user-%d", i), fmt.Sprintf("user-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				if err := leader.store.UpsertUser(ctx, user); err != nil {
					t.Fatal(err)
				}
				if err := creds.SetPassword(ctx, credential.SetPasswordRequest{UserID: user.ID(), Password: browserPassword, Cost: 4}); err != nil {
					t.Fatal(err)
				}
			}
			h := mainHandlerOf(t, cfg, leader.store, zerolog.Nop(), leader.leadership, nil)
			return func(attempt int, _ string) syncOutcome {
				form := url.Values{"subject": {fmt.Sprintf("user-%d", attempt)}, "password": {"wrong-password"}}
				r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", formType)
				r.RemoteAddr = net.JoinHostPort(client, "40000")
				r.Header.Set("X-Forwarded-For", forged(attempt))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				switch w.Code {
				case http.StatusSeeOther:
					return unauthenticated
				case http.StatusTooManyRequests:
					return limited
				}
				return syncOutcome(fmt.Sprintf("HTTP %d %s", w.Code, w.Body.String()))
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := tc.attempt(t)
			n := 0
			if tc.enroll {
				if got := attempt(n, "the-agent-token"); got != synced {
					t.Fatalf("enroll: %s, want %s", got, synced)
				}
				n++
			}
			for range 5 {
				if got := attempt(n, "stale-token"); got != unauthenticated {
					t.Fatalf("attempt %d, as %s: %s, want %s", n, forged(n), got, unauthenticated)
				}
				n++
			}
			if got := attempt(n, "the-agent-token"); got != limited {
				t.Fatalf("attempt %d, as %s after five failures as other addresses: %s, want %s: the forged header counted", n, forged(n), got, limited)
			}
		})
	}
}
