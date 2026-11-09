package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/handler"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

const (
	agentAuthDisabledWarning = "agent authentication disabled: any host that reaches the discovery listeners can register agents"
	openDiscoveryWarning     = "agent authentication covers enrolled agents only: the first sync of an unknown agent ID enrolls it and sets the endpoint podium calls, so set SOLTI_AGENT_AUTH_ENROLL_SECRET or tls.server.client_ca_file, or keep the discovery listeners reachable only from agent networks"
	// The startup warnings of a raft node about its forwards to the leader.
	plaintextForwardingWarning = "raft forwarding runs plaintext: without tls.server a follower forwards the writes it gets, logins, cookies and tokens included, and the agents' syncs to the leader over plain HTTP and gRPC; set tls.server on every node, and tls.client.ca_file to the CA of the nodes' server certificates"
	uncertifiedForwardWarning  = "agent mTLS is on (tls.server.client_ca_file) and tls.client.cert_file is unset: a follower presents no client certificate when it forwards an agent's sync to the leader, whose discovery listeners require one, so every sync that reaches a follower fails; set tls.client.cert_file and key_file to a certificate the agents' CA signed"
)

// forwardingWarnings returns the messages of the startup warnings about a raft
// node's forwards, found in a JSON log.
func forwardingWarnings(t *testing.T, logs []byte) []string {
	t.Helper()
	var out []string
	for _, line := range bytes.Split(bytes.TrimSpace(logs), []byte("\n")) {
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil || entry["level"] != "warn" {
			continue
		}
		if msg, _ := entry["message"].(string); msg == plaintextForwardingWarning || msg == uncertifiedForwardWarning {
			out = append(out, msg)
		}
	}
	return out
}

// A raft node says at start what its forwards to the leader lack, once per
// case: without tls.server a follower forwards logins, cookies and tokens in
// plaintext; with agent mTLS on and no tls.client certificate, it forwards an
// agent's sync without the client certificate the leader's discovery listeners
// require. TLS with a client certificate, or without agent mTLS, needs nothing.
func TestRaftNodeWarnsWhatItsForwardsLack(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		serverTLS, clientCA, cert bool
		want                      []string
	}{
		{name: "plaintext", want: []string{plaintextForwardingWarning}},
		{name: "tls", serverTLS: true},
		{name: "agent mTLS without a client certificate", serverTLS: true, clientCA: true, want: []string{uncertifiedForwardWarning}},
		{name: "agent mTLS with a client certificate", serverTLS: true, clientCA: true, cert: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			if tc.serverTLS {
				cfg.TLS.Server.CertFile, cfg.TLS.Server.KeyFile = "server.crt", "server.key"
			}
			if tc.clientCA {
				cfg.TLS.Server.ClientCAFile = "agents-ca.pem"
			}
			if tc.cert {
				cfg.TLS.Client.CertFile, cfg.TLS.Client.KeyFile = "client.crt", "client.key"
			}
			var logs bytes.Buffer
			warnForwarding(zerolog.New(&logs), cfg)
			if got := forwardingWarnings(t, logs.Bytes()); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("warnings %q, want %q", got, tc.want)
			}
		})
	}
}

// Standalone forwards nothing, and says nothing about forwards: a node without
// TLS, which warns in raft mode, starts without the warning.
func TestStandaloneWarnsOfNoForwarding(t *testing.T) {
	var logs bytes.Buffer
	cfg := config.Default()
	cfg.Cluster.Standalone.Ephemeral = true
	cfg.Auth.JWTSecret = strings.Repeat("standalone-forwarding-secret", 2)
	a, err := New(context.Background(), cfg, zerolog.New(&logs))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := forwardingWarnings(t, logs.Bytes()); len(got) != 0 {
		t.Fatalf("standalone warned %q", got)
	}
}

// discoveryWarnings returns the startup warnings about who can register
// agents, found in a JSON log.
func discoveryWarnings(t *testing.T, logs []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logs), []byte("\n")) {
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil || entry["level"] != "warn" {
			continue
		}
		if msg := entry["message"]; msg == agentAuthDisabledWarning || msg == openDiscoveryWarning {
			out = append(out, entry)
		}
	}
	return out
}

// Startup says who can register agents. With agent auth off, any host that
// reaches discovery. With it on, enrolled agents must present their token, but
// any host can still enroll a new agent ID unless an enrollment secret or
// client certificates keep it out.
func TestNewWarnsWhoCanRegisterAgents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		require bool
		want    string
	}{{"required", true, openDiscoveryWarning}, {"disabled", false, agentAuthDisabledWarning}} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			cfg := config.Default()
			cfg.Cluster.Standalone.Ephemeral = true
			cfg.Auth.JWTSecret = strings.Repeat("agent-auth-test-secret", 2)
			cfg.AgentAuth.Require = tc.require
			a, err := New(context.Background(), cfg, zerolog.New(&logs))
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			warnings := discoveryWarnings(t, logs.Bytes())
			if len(warnings) != 1 || warnings[0]["message"] != tc.want {
				t.Fatalf("discovery warnings %v, want one: %q", warnings, tc.want)
			}
			if tc.require && (warnings[0]["http_discovery_addr"] != ":8082" || warnings[0]["grpc_addr"] != ":50051") {
				t.Fatalf("warning %v does not name the discovery listeners :8082 and :50051", warnings[0])
			}
		})
	}
}

// Client certificates keep hosts without one out of discovery, so with agent
// auth on there is nothing to warn about.
func TestOpenDiscoveryWarningSkipsClientCertificates(t *testing.T) {
	cfg := config.Default()
	cfg.TLS.Server.ClientCAFile = "agents-ca.pem"
	var logs bytes.Buffer
	warnOpenDiscovery(zerolog.New(&logs), cfg)
	if warnings := discoveryWarnings(t, logs.Bytes()); len(warnings) != 0 {
		t.Fatalf("discovery warnings with client certificates required: %v", warnings)
	}

	cfg.AgentAuth.Require = false
	warnOpenDiscovery(zerolog.New(&logs), cfg)
	if warnings := discoveryWarnings(t, logs.Bytes()); len(warnings) != 1 || warnings[0]["message"] != agentAuthDisabledWarning {
		t.Fatalf("discovery warnings with agent auth off: %v, want %q", warnings, agentAuthDisabledWarning)
	}
}

// An enrollment secret keeps hosts without a derived token out of enrollment
// as client certificates keep out hosts without one: with agent auth on there
// is nothing to warn about. With agent auth off the secret does nothing, and
// the warning says so.
func TestOpenDiscoveryWarnSilencedBySecret(t *testing.T) {
	secret := strings.Repeat("s", 32)
	for _, tc := range []struct {
		name, secret, clientCA string
		require                bool
		want                   string // the one warning, or "" for none
	}{
		{name: "neither", require: true, want: openDiscoveryWarning},
		{name: "secret", secret: secret, require: true},
		{name: "client CA", clientCA: "agents-ca.pem", require: true},
		{name: "secret and client CA", secret: secret, clientCA: "agents-ca.pem", require: true},
		{name: "secret with agent auth off", secret: secret, want: agentAuthDisabledWarning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.AgentAuth.Require = tc.require
			cfg.AgentAuth.EnrollSecret = tc.secret
			cfg.TLS.Server.ClientCAFile = tc.clientCA
			var logs bytes.Buffer
			warnOpenDiscovery(zerolog.New(&logs), cfg)
			warnings := discoveryWarnings(t, logs.Bytes())
			switch {
			case tc.want == "" && len(warnings) != 0:
				t.Fatalf("discovery warnings %v, want none", warnings)
			case tc.want != "" && (len(warnings) != 1 || warnings[0]["message"] != tc.want):
				t.Fatalf("discovery warnings %v, want one: %q", warnings, tc.want)
			}
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("startup log shows the secret: %s", logs.String())
			}
		})
	}
}

// serveDiscovery serves srv on a loopback port and returns a client of it.
// The server sees the client as 127.0.0.1.
func serveDiscovery(t *testing.T, srv *grpc.Server) discoverv1.DiscoverServiceClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return discoverv1.NewDiscoverServiceClient(conn)
}

// discoveryClients syncs agents through both discovery listeners, as wired by
// initServices and buildDiscoveryListeners from a config, always as the client
// 127.0.0.1. agents is the service behind them.
type discoveryClients struct {
	http   http.Handler
	grpc   discoverv1.DiscoverServiceClient
	agents *agent.Service
}

// newDiscoveryClients serves discovery wired from the default config.
func newDiscoveryClients(t *testing.T) discoveryClients {
	t.Helper()
	return newDiscoveryClientsFor(t, config.Default())
}

// newDiscoveryClientsFor serves discovery wired from cfg the way [New] wires it.
func newDiscoveryClientsFor(t *testing.T, cfg config.Config) discoveryClients {
	t.Helper()
	return newDiscoveryClientsLogged(t, cfg, zerolog.Nop())
}

// newDiscoveryClientsLogged is newDiscoveryClientsFor with everything logging to logger.
func newDiscoveryClientsLogged(t *testing.T, cfg config.Config, logger zerolog.Logger) discoveryClients {
	t.Helper()
	store := inmemory.New()
	svc, err := initServices(store, kit.New(store, cfg.Auth), cfg.History.Archive, cfg.AgentAuth, logger)
	if err != nil {
		t.Fatal(err)
	}
	hub := event.NewHub(logger)
	t.Cleanup(hub.Close)
	h, srv := buildDiscoveryListeners(logger, svc.agent, hub, standalone.NewLeadership(), cfg, nil, nil, nil, nil)
	return discoveryClients{http: h, grpc: serveDiscovery(t, srv), agents: svc.agent}
}

func (c discoveryClients) syncHTTP(id, token string) int {
	body := `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	c.http.ServeHTTP(w, r)
	return w.Code
}

func (c discoveryClients) syncGRPC(id, token string) error {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	_, err := c.grpc.Sync(ctx, &discoverv1.SyncRequest{
		Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1,
	})
	return err
}

// Discovery messages over the HTTP sync body limit never reach the handler.
func TestGRPCDiscoveryRejectsOversizedMessage(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	client := serveDiscovery(t, buildGRPCServer(logger, svc, hub, standalone.NewLeadership(), kit.NewLimiter(auth.Config{}), handler.NewSyncBuckets(), nil, nil, false, 0, nil))
	sync := func(id string, osBytes int) error {
		_, err := client.Sync(ctx, &discoverv1.SyncRequest{
			Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1,
			Os: strings.Repeat("x", osBytes),
		})
		return err
	}
	if err := sync("small-agent", 1<<10); err != nil {
		t.Fatalf("regular sync rejected: %v", err)
	}
	if err := sync("large-agent", 300<<10); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("300 KiB sync: %v, want ResourceExhausted", err)
	}
	if _, err := svc.Get(ctx, "large-agent"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("oversized sync reached the handler: %v", err)
	}
}

// The gRPC discovery listener caps request headers at 16 KiB, like the HTTP
// listeners: larger metadata never reaches the handler.
func TestGRPCDiscoveryCapsHeaders(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	client := serveDiscovery(t, buildGRPCServer(logger, svc, hub, standalone.NewLeadership(), kit.NewLimiter(auth.Config{}), handler.NewSyncBuckets(), nil, nil, false, 0, nil))
	sync := func(id string, padding int) error {
		md := metadata.AppendToOutgoingContext(ctx, "x-padding", strings.Repeat("p", padding))
		_, err := client.Sync(md, &discoverv1.SyncRequest{
			Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1,
		})
		return err
	}
	if err := sync("small-agent", 12<<10); err != nil {
		t.Fatalf("sync with 12 KiB of metadata: %v", err)
	}
	if err := sync("large-agent", 20<<10); err == nil {
		t.Fatal("sync with 20 KiB of metadata accepted, want it refused")
	}
	if _, err := svc.Get(ctx, "large-agent"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("oversized headers reached the handler: %v", err)
	}
}

// The discovery listeners have no per-IP limit: agents behind one egress IP
// are blocked one by one, never together.
func TestDiscoveryListenersBlockOnlyTheFailingAgent(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		c := newDiscoveryClients(t)
		for _, id := range []string{"agent-a", "agent-b"} {
			if code := c.syncHTTP(id, "token-"+id); code != http.StatusOK {
				t.Fatalf("enroll %s: HTTP %d", id, code)
			}
		}
		for i := range 5 {
			if code := c.syncHTTP("agent-a", "stale-token"); code != http.StatusUnauthorized {
				t.Fatalf("bad token %d: HTTP %d, want 401", i+1, code)
			}
		}
		if code := c.syncHTTP("agent-b", "token-agent-b"); code != http.StatusOK {
			t.Fatalf("agent-b behind the same IP: HTTP %d, want 200", code)
		}
		if code := c.syncHTTP("agent-a", "token-agent-a"); code != http.StatusTooManyRequests {
			t.Fatalf("agent-a after five failures: HTTP %d, want 429", code)
		}
	})

	t.Run("grpc", func(t *testing.T) {
		c := newDiscoveryClients(t)
		for _, id := range []string{"agent-a", "agent-b"} {
			if err := c.syncGRPC(id, "token-"+id); err != nil {
				t.Fatalf("enroll %s: %v", id, err)
			}
		}
		for i := range 5 {
			if err := c.syncGRPC("agent-a", "stale-token"); status.Code(err) != codes.Unauthenticated {
				t.Fatalf("bad token %d: %v, want Unauthenticated", i+1, err)
			}
		}
		if err := c.syncGRPC("agent-b", "token-agent-b"); err != nil {
			t.Fatalf("agent-b behind the same peer: %v", err)
		}
		if err := c.syncGRPC("agent-a", "token-agent-a"); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("agent-a after five failures: %v, want ResourceExhausted", err)
		}
	})
}

// HTTP and gRPC discovery count token failures in one budget per (client IP,
// agent ID): failures over one transport count toward the other.
func TestDiscoveryTransportsShareTheFailureBudget(t *testing.T) {
	c := newDiscoveryClients(t)
	if err := c.syncGRPC("agent-a", "token-agent-a"); err != nil {
		t.Fatalf("enroll agent-a: %v", err)
	}
	for i := range 3 {
		if code := c.syncHTTP("agent-a", "stale-token"); code != http.StatusUnauthorized {
			t.Fatalf("bad token %d over HTTP: HTTP %d, want 401", i+1, code)
		}
	}
	for i := range 2 {
		if err := c.syncGRPC("agent-a", "stale-token"); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("bad token %d over gRPC: %v, want Unauthenticated", i+1, err)
		}
	}
	if err := c.syncGRPC("agent-a", "token-agent-a"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("gRPC after three HTTP and two gRPC failures: %v, want ResourceExhausted", err)
	}
	if code := c.syncHTTP("agent-a", "token-agent-a"); code != http.StatusTooManyRequests {
		t.Fatalf("HTTP after the shared budget ran out: HTTP %d, want 429", code)
	}
}

// HTTP and gRPC discovery draw on one bucket of syncs per agent ID: an agent
// that alternates between the transports does not get a burst on each. The
// listeners keep the real clock, so a second that passes during the test may
// refill a token: the syncs that land can exceed the burst by at most the whole
// seconds the test took.
func TestDiscoveryTransportsShareTheSyncBudget(t *testing.T) {
	const burst, attempts = 10, 14 // seven syncs over each transport
	c := newDiscoveryClients(t)
	start := time.Now()
	landed := 0
	for i := range attempts {
		var refused bool
		if i%2 == 0 {
			switch code := c.syncHTTP("agent-a", "token-agent-a"); code {
			case http.StatusOK:
			case http.StatusTooManyRequests:
				refused = true
			default:
				t.Fatalf("sync %d over HTTP: HTTP %d", i+1, code)
			}
		} else {
			switch err := c.syncGRPC("agent-a", "token-agent-a"); status.Code(err) {
			case codes.OK:
			case codes.ResourceExhausted:
				refused = true
			default:
				t.Fatalf("sync %d over gRPC: %v", i+1, err)
			}
		}
		if !refused {
			landed++
		}
	}
	if most := burst + int(time.Since(start)/time.Second); landed < burst || landed > most {
		t.Fatalf("%d of %d syncs over both transports landed, want from %d to %d (one bucket for both)", landed, attempts, burst, most)
	}
}

// accessLevels counts the lines of a JSON log that say message, by level.
func accessLevels(logs *syncBuffer, message string) map[string]int {
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry struct{ Level, Message string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Message == message {
			counts[entry.Level]++
		}
	}
	return counts
}

// A sync that the rate refuses is logged by the access loggers at DEBUG, not as
// the WARN of an HTTP 4xx or the ERROR of a failed gRPC call: one DEBUG line for
// each refused sync, so the level changes and not the number of lines. A sync
// that fails otherwise keeps its level: the 401 / Unauthenticated of a wrong
// token, and the 429 / ResourceExhausted of the failure limiter, which is no
// throttle of the rate. The handler's own WARN is not an access line.
func TestThrottledSyncsAreLoggedAtDebug(t *testing.T) {
	const burst, attempts = 10, 40
	const ok, limited, unauthenticated = "ok", "limited", "unauthenticated"
	for _, tr := range []struct {
		name    string
		message string // of the access line
		failed  string // the level of a failed call
		sync    func(c discoveryClients, id, token string) string
	}{
		{"http", "http request", "warn", func(c discoveryClients, id, token string) string {
			switch code := c.syncHTTP(id, token); code {
			case http.StatusOK:
				return ok
			case http.StatusTooManyRequests:
				return limited
			case http.StatusUnauthorized:
				return unauthenticated
			default:
				return fmt.Sprintf("HTTP %d", code)
			}
		}},
		{"grpc", "grpc request", "error", func(c discoveryClients, id, token string) string {
			switch err := c.syncGRPC(id, token); status.Code(err) {
			case codes.OK:
				return ok
			case codes.ResourceExhausted:
				return limited
			case codes.Unauthenticated:
				return unauthenticated
			default:
				return err.Error()
			}
		}},
	} {
		t.Run(tr.name, func(t *testing.T) {
			logs := &syncBuffer{}
			c := newDiscoveryClientsLogged(t, config.Default(), zerolog.New(logs))
			lines := func() map[string]int { return accessLevels(logs, tr.message) }

			// A burst of one agent: the first ten land, the rest are throttled.
			before := lines()
			landed, throttled := 0, 0
			for i := range attempts {
				switch got := tr.sync(c, "agent-a", "token-a"); got {
				case ok:
					landed++
				case limited:
					throttled++
				default:
					t.Fatalf("sync %d of the burst: %s", i+1, got)
				}
			}
			after := lines()
			if after["warn"] != before["warn"] || after["error"] != before["error"] {
				t.Fatalf("the burst logged %d WARN and %d ERROR access lines, want none", after["warn"]-before["warn"], after["error"]-before["error"])
			}
			if got := after["debug"] - before["debug"]; got != throttled || throttled == 0 {
				t.Fatalf("%d throttled syncs logged %d DEBUG access lines, want one each", throttled, got)
			}
			if got := after["info"] - before["info"]; got != landed || landed < burst {
				t.Fatalf("%d accepted syncs logged %d INFO access lines, want one each, at least %d", landed, got, burst)
			}

			// A wrong token for an agent whose bucket is whole still logs a failure.
			if got := tr.sync(c, "agent-b", "token-b"); got != ok {
				t.Fatalf("enroll agent-b: %s", got)
			}
			before = lines()
			if got := tr.sync(c, "agent-b", "stale-token"); got != unauthenticated {
				t.Fatalf("a wrong token: %s, want %s", got, unauthenticated)
			}
			if got := lines()[tr.failed] - before[tr.failed]; got != 1 {
				t.Fatalf("a wrong token logged %d access lines at %s, want 1", got, tr.failed)
			}

			// The failure limiter's 429 / ResourceExhausted is no throttle of the rate.
			if got := tr.sync(c, "agent-c", "token-c"); got != ok {
				t.Fatalf("enroll agent-c: %s", got)
			}
			for range 5 {
				tr.sync(c, "agent-c", "stale-token")
			}
			before = lines()
			if got := tr.sync(c, "agent-c", "token-c"); got != limited {
				t.Fatalf("agent-c after five failures: %s, want %s", got, limited)
			}
			if got := lines()[tr.failed] - before[tr.failed]; got != 1 {
				t.Fatalf("a sync blocked by the failure limiter logged %d access lines at %s, want 1", got, tr.failed)
			}
		})
	}
}

// The enrollment secret reaches the agent service through the composition
// root: on both listeners an unknown agent ID enrolls only with the token
// derived from agent_auth's secret, and anything else is refused and stores
// nothing.
func TestDiscoveryEnrollmentUsesTheConfiguredSecret(t *testing.T) {
	const (
		secret = "0123456789abcdef0123456789abcdef"
		token1 = "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058" // HMAC-SHA256(secret, "agent-1"), hex
		token2 = "14202fea7ea2cc313572d5d8e8c424489c954421b0f6160ce9e07d27340644fc" // the same for "agent-2"
	)
	ctx := context.Background()
	cfg := config.Default()
	cfg.AgentAuth.EnrollSecret = secret
	c := newDiscoveryClientsFor(t, cfg)

	if code := c.syncHTTP("agent-1", "made-up-token"); code != http.StatusUnauthorized {
		t.Fatalf("HTTP enroll with a made-up token: HTTP %d, want 401", code)
	}
	if err := c.syncGRPC("agent-2", "made-up-token"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("gRPC enroll with a made-up token: %v, want Unauthenticated", err)
	}
	for _, id := range []string{"agent-1", "agent-2"} {
		if token, err := c.agents.AgentToken(ctx, id); err != nil || token != "" {
			t.Fatalf("refused agent %s enrolled a token: %q %v", id, token, err)
		}
		if _, err := c.agents.Get(ctx, id); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("refused agent %s was stored: %v", id, err)
		}
	}

	if code := c.syncHTTP("agent-1", token1); code != http.StatusOK {
		t.Fatalf("HTTP enroll with the derived token: HTTP %d, want 200", code)
	}
	if err := c.syncGRPC("agent-2", token2); err != nil {
		t.Fatalf("gRPC enroll with the derived token: %v", err)
	}
	for id, want := range map[string]string{"agent-1": token1, "agent-2": token2} {
		if token, err := c.agents.AgentToken(ctx, id); err != nil || token != want {
			t.Fatalf("stored token of %s: %q %v, want its derived token", id, token, err)
		}
	}
}

// Standalone gRPC discovery takes its client IP from the peer, as it did before
// followers forwarded syncs: x-forwarded-for counts for nothing, even from a
// host that trusted_proxies lists, while HTTP discovery believes that host's
// X-Forwarded-For. Each attempt names an address of its own; over gRPC the
// limiter still counts all of them at the peer.
func TestStandaloneGRPCDiscoveryIgnoresForwardedFor(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxies = []string{"127.0.0.1"} // the address of the test's own client
	logger := zerolog.Nop()
	leadership := standalone.NewLeadership()
	resolver, closeHosts, err := clientResolver(cfg, leadership)
	if err != nil || closeHosts != nil {
		t.Fatalf("clientResolver in standalone: %v, with a closer: %v", err, closeHosts != nil)
	}
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	t.Cleanup(hub.Close)
	h, srv := buildDiscoveryListeners(logger, svc, hub, leadership, cfg, resolver, nil, nil, nil)
	client := serveDiscovery(t, srv)
	forged := func(attempt int) string { return fmt.Sprintf("198.51.100.%d", attempt+1) }
	for _, tc := range []struct {
		name string
		sync func(attempt int, id, token string) error
		want func(err error) bool // the end of the sixth attempt, after five failures
	}{
		{"grpc", func(attempt int, id, token string) error {
			ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token, "x-forwarded-for", forged(attempt))
			_, err := client.Sync(ctx, &discoverv1.SyncRequest{Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
			return err
		}, func(err error) bool { return status.Code(err) == codes.ResourceExhausted }},
		{"http", func(attempt int, id, token string) error {
			body := `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
			r := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
			r.RemoteAddr = "127.0.0.1:40000"
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Forwarded-For", forged(attempt))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				return fmt.Errorf("HTTP %d", w.Code)
			}
			return nil
		}, func(err error) bool { return err == nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "agent-" + tc.name
			if err := tc.sync(0, id, "token-"+id); err != nil {
				t.Fatalf("enroll: %v", err)
			}
			for attempt := 1; attempt <= 5; attempt++ {
				if err := tc.sync(attempt, id, "stale-token"); err == nil {
					t.Fatalf("bad token %d accepted", attempt)
				}
			}
			if err := tc.sync(6, id, "token-"+id); !tc.want(err) {
				t.Fatalf("the sixth attempt, as %s: %v", forged(6), err)
			}
		})
	}
}

// Agents are not browsers: the discovery listener has no cross-origin check,
// so browser headers on a sync change nothing.
func TestDiscoveryListenerSkipsCrossOriginCheck(t *testing.T) {
	c := newDiscoveryClients(t)
	body := `{"id":"agent-a","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set("Authorization", "Bearer token-agent-a")
	r.Header.Set("Origin", "https://other.example.com")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	c.http.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("cross-site sync: HTTP %d %s, want 200", w.Code, w.Body.String())
	}
}
