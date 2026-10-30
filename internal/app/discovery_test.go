package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

const (
	agentAuthDisabledWarning = "agent authentication disabled: any host that reaches the discovery listeners can register agents"
	openDiscoveryWarning     = "agent authentication covers enrolled agents only: the first sync of an unknown agent ID enrolls it and sets the endpoint podium calls, so set SOLTI_AGENT_AUTH_ENROLL_SECRET or tls.server.client_ca_file, or keep the discovery listeners reachable only from agent networks"
)

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
	logger := zerolog.Nop()
	store := inmemory.New()
	svc, err := initServices(store, kit.New(store, cfg.Auth), cfg.History.Archive, cfg.AgentAuth, logger)
	if err != nil {
		t.Fatal(err)
	}
	hub := event.NewHub(logger)
	t.Cleanup(hub.Close)
	h, srv := buildDiscoveryListeners(logger, svc.agent, hub, standalone.NewLeadership(), cfg, nil, nil)
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
	client := serveDiscovery(t, buildGRPCServer(logger, svc, hub, standalone.NewLeadership(), kit.NewLimiter(auth.Config{}), nil, false))
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
	client := serveDiscovery(t, buildGRPCServer(logger, svc, hub, standalone.NewLeadership(), kit.NewLimiter(auth.Config{}), nil, false))
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
