package handler

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// syncHTTP posts one proto-JSON heartbeat, with a bearer token when token is set.
func syncHTTP(h *HTTPDiscovery, body, token string) *httptest.ResponseRecorder {
	return syncHTTPFrom(h, "192.0.2.1:1234", body, token)
}

// syncHTTPFrom is syncHTTP from the TCP peer remoteAddr.
func syncHTTPFrom(h *HTTPDiscovery, remoteAddr, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res := httptest.NewRecorder()
	h.Sync(res, req)
	return res
}

// discoveryLimiter blocks an (IP, agent ID) pair after five authentication failures.
func discoveryLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{MaxAttempts: 5, Window: time.Minute, BlockWindow: time.Minute})
}

// assertNotRegistered checks that a rejected sync neither enrolled a token nor stored the agent.
func assertNotRegistered(t *testing.T, svc *agent.Service, id string) {
	t.Helper()
	ctx := context.Background()
	if token, err := svc.AgentToken(ctx, id); err != nil || token != "" {
		t.Fatalf("rejected agent %q enrolled a token: %q %v", id, token, err)
	}
	if _, err := svc.Get(ctx, id); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("rejected agent %q was stored: %v", id, err)
	}
}

func TestHTTPDiscoveryRejectsInvalidAgent(t *testing.T) {
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	h := NewHTTPDiscovery(logger, svc, hub, discoveryLimiter(), nil, true)
	for _, tc := range []struct{ name, id, body, message string }{
		{"unsafe id", `x'"`, `{"id":"x'\"","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`, "Invalid agent data: agent id may contain only [A-Za-z0-9._-]."},
		{"unparsable endpoint", "agent-1", `{"id":"agent-1","endpoint":"http://[","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`, "Invalid agent data: agent endpoint is not a valid URL."},
		{"endpoint query", "agent-1", `{"id":"agent-1","endpoint":"http://10.0.0.5:8085?x=1","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`, "Invalid agent data: agent endpoint must not contain a query or fragment."},
		{"heartbeat over one hour", "agent-1", `{"id":"agent-1","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1,"heartbeatIntervalS":3601}`, "Invalid agent data: heartbeat interval must be 0 or 1-3600 seconds."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := syncHTTP(h, tc.body, "agent-token")
			if res.Code != http.StatusBadRequest {
				t.Fatalf("invalid agent sync: HTTP %d %s, want 400", res.Code, res.Body.String())
			}
			// The answer names the broken rule, without the error's sentinel prefix.
			if got := responseMessage(t, res); got != tc.message {
				t.Fatalf("invalid agent sync answered %q, want %q", got, tc.message)
			}
			assertNotRegistered(t, svc, tc.id)
		})
	}
}

// responseMessage is the message of a JSON error response.
func responseMessage(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("error response %q is not JSON: %v", res.Body.String(), err)
	}
	return body.Message
}

func TestGRPCDiscoveryRejectsInvalidAgent(t *testing.T) {
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	g := NewGRPCDiscovery(logger, svc, hub, discoveryLimiter(), true)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer agent-token"))
	tooMuchMetadata := make(map[string]string, 65)
	for i := range 65 {
		tooMuchMetadata[fmt.Sprintf("key-%d", i)] = "v"
	}
	for _, tc := range []struct {
		name    string
		req     *discoverv1.SyncRequest
		message string
	}{
		{"unsafe id", &discoverv1.SyncRequest{Id: `x'"`, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1}, "invalid agent data: agent id may contain only [A-Za-z0-9._-]"},
		{"resolver scheme endpoint", &discoverv1.SyncRequest{Id: "agent-1", Endpoint: "unix:///run/agent.sock", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1}, "invalid agent data: agent endpoint port must be 1-65535"},
		{"too much metadata", &discoverv1.SyncRequest{Id: "agent-1", Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1, Metadata: tooMuchMetadata}, "invalid agent data: agent metadata exceeds 64 entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.Sync(ctx, tc.req)
			if grpcstatus.Code(err) != codes.InvalidArgument {
				t.Fatalf("invalid agent sync: %v, want InvalidArgument", err)
			}
			// The answer names the broken rule, without the error's sentinel prefix.
			if msg := grpcstatus.Convert(err).Message(); msg != tc.message {
				t.Fatalf("invalid agent sync answered %q, want %q", msg, tc.message)
			}
			assertNotRegistered(t, svc, tc.req.GetId())
		})
	}
}

func TestDiscoveryRequiresTokenByDefault(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	requireAuth := config.Default().AgentAuth.Require
	h := NewHTTPDiscovery(logger, svc, hub, discoveryLimiter(), nil, requireAuth)
	body := func(endpoint string) string {
		return `{"id":"enrolled-agent","name":"Agent","endpoint":"` + endpoint + `","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
	}
	if res := syncHTTP(h, body("http://10.0.0.5:8085"), ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless sync with default config: HTTP %d, want 401", res.Code)
	}
	if res := syncHTTP(h, body("http://10.0.0.5:8085"), "enrolled-token"); res.Code != http.StatusOK {
		t.Fatalf("first sync with a token: HTTP %d %s", res.Code, res.Body.String())
	}
	if res := syncHTTP(h, body("http://203.0.113.7:8085"), "other-token"); res.Code != http.StatusUnauthorized {
		t.Fatalf("re-sync with a different token: HTTP %d, want 401", res.Code)
	}
	enrolled, err := svc.Get(ctx, "enrolled-agent")
	if err != nil {
		t.Fatal(err)
	}
	if enrolled.Endpoint() != "http://10.0.0.5:8085" {
		t.Fatalf("re-sync with a different token replaced the endpoint: %s", enrolled.Endpoint())
	}
	g := NewGRPCDiscovery(logger, svc, hub, discoveryLimiter(), requireAuth)
	_, err = g.Sync(ctx, &discoverv1.SyncRequest{Id: "grpc-agent", Endpoint: "10.0.0.6:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
	if grpcstatus.Code(err) != codes.Unauthenticated {
		t.Fatalf("tokenless gRPC sync with default config: %v, want Unauthenticated", err)
	}
}

func TestDiscoveryAcceptsSDKProtoJSONCapabilitiesAndRejectsOversize(t *testing.T) {
	store := inmemory.New()
	logger := zerolog.Nop()
	svc := agent.New(store, logger)
	hub := event.NewHub(logger)
	handler := NewHTTPDiscovery(logger, svc, hub, discoveryLimiter(), nil, false)
	body := `{"id":"sdk-agent","name":"SDK Agent","endpoint":"http://127.0.0.1:1234","uptimeSeconds":"5","ts":"100","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1,"heartbeatIntervalS":30,"capabilities":{"runners":[{"name":"default","labels":{"zone":"eu"},"workloads":[{"apiVersion":"solti.io/v1","kind":"Subprocess"}]}]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.Sync(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("SDK discovery rejected: %d %s", res.Code, res.Body.String())
	}
	found, err := svc.Get(context.Background(), "sdk-agent")
	if err != nil {
		t.Fatal(err)
	}
	caps := found.Capabilities()
	if len(caps) != 1 || caps[0].Workloads[0].Group != "solti.io" || caps[0].Workloads[0].Kind != "Subprocess" || caps[0].Labels["zone"] != "eu" {
		t.Fatalf("capabilities lost: %+v", caps)
	}
	// A valid JSON prefix padded past the ceiling must never be accepted.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body+strings.Repeat(" ", MaxSyncBytes)))
	res = httptest.NewRecorder()
	handler.Sync(res, req)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize valid prefix accepted: %d", res.Code)
	}
}

// Agents behind one egress IP (NAT, Kubernetes) must not block each other:
// failures count per (client IP, agent ID).
func TestDiscoveryAuthFailuresBlockOnlyThatAgent(t *testing.T) {
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	h := NewHTTPDiscovery(logger, svc, hub, discoveryLimiter(), nil, true)
	body := func(id string) string {
		return `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
	}
	const egress, otherIP = "203.0.113.9:40000", "198.51.100.7:40000"
	for _, id := range []string{"agent-a", "agent-b"} {
		if res := syncHTTPFrom(h, egress, body(id), "token-"+id); res.Code != http.StatusOK {
			t.Fatalf("enroll %s: HTTP %d %s", id, res.Code, res.Body.String())
		}
	}
	for i := range 5 {
		if res := syncHTTPFrom(h, egress, body("agent-a"), "stale-token"); res.Code != http.StatusUnauthorized {
			t.Fatalf("bad token %d: HTTP %d, want 401", i+1, res.Code)
		}
	}
	if res := syncHTTPFrom(h, egress, body("agent-a"), "token-agent-a"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("agent-a after five failures: HTTP %d, want 429", res.Code)
	}
	if res := syncHTTPFrom(h, egress, body("agent-b"), "token-agent-b"); res.Code != http.StatusOK {
		t.Fatalf("agent-b behind the same IP: HTTP %d %s, want 200", res.Code, res.Body.String())
	}
	if res := syncHTTPFrom(h, otherIP, body("agent-a"), "token-agent-a"); res.Code != http.StatusOK {
		t.Fatalf("agent-a from another IP: HTTP %d %s, want 200", res.Code, res.Body.String())
	}
}

func TestGRPCDiscoveryAuthFailuresBlockOnlyThatAgent(t *testing.T) {
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	g := NewGRPCDiscovery(logger, svc, hub, discoveryLimiter(), true)
	sync := func(peerIP, id, token string) error {
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(peerIP), Port: 40000}})
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
		_, err := g.Sync(ctx, &discoverv1.SyncRequest{Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
		return err
	}
	const egress, otherIP = "203.0.113.9", "198.51.100.7"
	for _, id := range []string{"agent-a", "agent-b"} {
		if err := sync(egress, id, "token-"+id); err != nil {
			t.Fatalf("enroll %s: %v", id, err)
		}
	}
	for i := range 5 {
		if err := sync(egress, "agent-a", "stale-token"); grpcstatus.Code(err) != codes.Unauthenticated {
			t.Fatalf("bad token %d: %v, want Unauthenticated", i+1, err)
		}
	}
	if err := sync(egress, "agent-a", "token-agent-a"); grpcstatus.Code(err) != codes.ResourceExhausted {
		t.Fatalf("agent-a after five failures: %v, want ResourceExhausted", err)
	}
	if err := sync(egress, "agent-b", "token-agent-b"); err != nil {
		t.Fatalf("agent-b behind the same IP: %v", err)
	}
	if err := sync(otherIP, "agent-a", "token-agent-a"); err != nil {
		t.Fatalf("agent-a from another IP: %v", err)
	}
}

// gatedTx holds each transaction, once armed, until release is closed. A
// bad-token sync runs exactly one transaction: its token check.
type gatedTx struct {
	storage.Storage
	armed   atomic.Bool
	checks  atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (g *gatedTx) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	if g.armed.Load() {
		g.checks.Add(1)
		g.entered <- struct{}{}
		<-g.release
	}
	return g.Storage.WithTx(ctx, fn)
}

type syncOutcome string

const (
	synced          syncOutcome = "synced"
	refused         syncOutcome = "refused"
	unauthenticated syncOutcome = "unauthenticated"
)

// Concurrent bad-token syncs for one (IP, agent ID) reserve their attempt
// before the token check, so at most rate_attempts checks ever run.
func TestDiscoveryBoundsConcurrentTokenChecks(t *testing.T) {
	const attempts, limit = 20, 5 // limit is discoveryLimiter's MaxAttempts
	for _, tc := range []struct {
		name    string
		handler func(svc *agent.Service, hub *event.Hub) func(token string) syncOutcome
	}{
		{"http", func(svc *agent.Service, hub *event.Hub) func(string) syncOutcome {
			h := NewHTTPDiscovery(zerolog.Nop(), svc, hub, discoveryLimiter(), nil, true)
			body := `{"id":"agent-a","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
			return func(token string) syncOutcome {
				switch code := syncHTTPFrom(h, "203.0.113.9:40000", body, token).Code; code {
				case http.StatusOK:
					return synced
				case http.StatusTooManyRequests:
					return refused
				case http.StatusUnauthorized:
					return unauthenticated
				default:
					return syncOutcome(fmt.Sprintf("HTTP %d", code))
				}
			}
		}},
		{"grpc", func(svc *agent.Service, hub *event.Hub) func(string) syncOutcome {
			g := NewGRPCDiscovery(zerolog.Nop(), svc, hub, discoveryLimiter(), true)
			from := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 40000}})
			return func(token string) syncOutcome {
				ctx := metadata.NewIncomingContext(from, metadata.Pairs("authorization", "Bearer "+token))
				_, err := g.Sync(ctx, &discoverv1.SyncRequest{Id: "agent-a", Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
				switch grpcstatus.Code(err) {
				case codes.OK:
					return synced
				case codes.ResourceExhausted:
					return refused
				case codes.Unauthenticated:
					return unauthenticated
				default:
					return syncOutcome(err.Error())
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger := zerolog.Nop()
			store := &gatedTx{Storage: inmemory.New(), entered: make(chan struct{}, attempts), release: make(chan struct{})}
			hub := event.NewHub(logger)
			defer hub.Close()
			sync := tc.handler(agent.New(store, logger), hub)
			if got := sync("token-a"); got != synced {
				t.Fatalf("enroll agent-a: %s", got)
			}

			store.armed.Store(true)
			results := make(chan syncOutcome, attempts)
			for range attempts {
				go func() { results <- sync("stale-token") }()
			}
			// Each sync either waits inside its token check or comes back refused.
			admitted := 0
			for range attempts {
				select {
				case <-store.entered:
					admitted++
				case got := <-results:
					if got != refused {
						t.Errorf("sync that skipped the token check: %s, want refused", got)
					}
				}
			}
			if admitted > limit {
				t.Errorf("%d concurrent token checks ran, want at most %d", admitted, limit)
			}

			close(store.release)
			for range admitted {
				if got := <-results; got != unauthenticated {
					t.Errorf("bad token: %s, want unauthenticated", got)
				}
			}
			// The pair is blocked: even the right token is refused before any check.
			checks := store.checks.Load()
			if got := sync("token-a"); got != refused {
				t.Fatalf("agent-a after the burst: %s, want refused", got)
			}
			if store.checks.Load() != checks {
				t.Fatal("a sync of the blocked pair ran a token check")
			}
		})
	}
}

// unavailableTx fails every transaction, once armed, as an unreachable store would.
type unavailableTx struct {
	storage.Storage
	armed atomic.Bool
}

func (u *unavailableTx) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	if u.armed.Load() {
		return storage.ErrUnavailable
	}
	return u.Storage.WithTx(ctx, fn)
}

// A token check that fails for any reason but a wrong token releases its
// attempt: storage outages never block an agent.
func TestDiscoveryStorageErrorsDoNotCountAsFailures(t *testing.T) {
	logger := zerolog.Nop()
	store := &unavailableTx{Storage: inmemory.New()}
	hub := event.NewHub(logger)
	defer hub.Close()
	h := NewHTTPDiscovery(logger, agent.New(store, logger), hub, discoveryLimiter(), nil, true)
	body := `{"id":"agent-a","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
	if res := syncHTTP(h, body, "token-a"); res.Code != http.StatusOK {
		t.Fatalf("enroll agent-a: HTTP %d", res.Code)
	}
	store.armed.Store(true)
	for i := range 10 {
		if res := syncHTTP(h, body, "token-a"); res.Code != http.StatusServiceUnavailable {
			t.Fatalf("sync %d with the store down: HTTP %d, want 503", i+1, res.Code)
		}
	}
	store.armed.Store(false)
	if res := syncHTTP(h, body, "token-a"); res.Code != http.StatusOK {
		t.Fatalf("sync after the outage: HTTP %d, want 200", res.Code)
	}
}

// An agent token over 1024 bytes is refused before the limiter and the store
// see it: HTTP 400, gRPC InvalidArgument. An unknown agent is not enrolled,
// and the refusals count as no failure: more of them than rate_attempts leave
// an enrolled agent unblocked.
func TestDiscoveryRejectsOverlongToken(t *testing.T) {
	overlong, longest := strings.Repeat("t", 1025), strings.Repeat("t", 1024)
	logger := zerolog.Nop()

	t.Run("http", func(t *testing.T) {
		svc := agent.New(inmemory.New(), logger)
		hub := event.NewHub(logger)
		defer hub.Close()
		h := NewHTTPDiscovery(logger, svc, hub, discoveryLimiter(), nil, true)
		body := func(id string) string {
			return `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
		}
		refused := func(id string) {
			t.Helper()
			res := syncHTTP(h, body(id), overlong)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("overlong token for %s: HTTP %d %s, want 400", id, res.Code, res.Body.String())
			}
			if got := responseMessage(t, res); got != "Agent token is too long." {
				t.Fatalf("overlong token for %s answered %q, want %q", id, got, "Agent token is too long.")
			}
		}
		refused("agent-b")
		assertNotRegistered(t, svc, "agent-b")

		if res := syncHTTP(h, body("agent-a"), longest); res.Code != http.StatusOK {
			t.Fatalf("enroll a 1024-byte token: HTTP %d %s, want 200", res.Code, res.Body.String())
		}
		for range 6 {
			refused("agent-a")
		}
		if res := syncHTTP(h, body("agent-a"), longest); res.Code != http.StatusOK {
			t.Fatalf("enrolled token after the refusals: HTTP %d %s, want 200", res.Code, res.Body.String())
		}
	})

	t.Run("grpc", func(t *testing.T) {
		svc := agent.New(inmemory.New(), logger)
		hub := event.NewHub(logger)
		defer hub.Close()
		g := NewGRPCDiscovery(logger, svc, hub, discoveryLimiter(), true)
		sync := func(id, token string) error {
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
			_, err := g.Sync(ctx, &discoverv1.SyncRequest{Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
			return err
		}
		refused := func(id string) {
			t.Helper()
			err := sync(id, overlong)
			if grpcstatus.Code(err) != codes.InvalidArgument || grpcstatus.Convert(err).Message() != "agent token is too long" {
				t.Fatalf("overlong token for %s: %v, want InvalidArgument: agent token is too long", id, err)
			}
		}
		refused("agent-b")
		assertNotRegistered(t, svc, "agent-b")

		if err := sync("agent-a", longest); err != nil {
			t.Fatalf("enroll a 1024-byte token: %v", err)
		}
		for range 6 {
			refused("agent-a")
		}
		if err := sync("agent-a", longest); err != nil {
			t.Fatalf("enrolled token after the refusals: %v", err)
		}
	})
}

// A sync without a token while agent auth is required is refused and logged
// at WARN with the agent ID and the ways out. With an enrollment secret the
// ways out leave agent_auth.require=false aside, which fails validation next to
// a secret. They point to podium agent-token for an ID that is not enrolled
// yet, and tell an agent enrolled earlier to keep sending its existing token
// and fix its token transport: a derived token that differs locks it out. Only
// attempts the limiter admits log, so a pair logs at most rate_attempts lines
// per window.
func TestTokenlessSyncWarns(t *testing.T) {
	const (
		secret  = "0123456789abcdef0123456789abcdef"
		warning = "agent sent no token: serve discovery over TLS so the SDK sends it, " +
			"call allow_insecure_token_transport() in the SDK, or set agent_auth.require=false"
		warningWithSecret = "agent sent no token: give an agent ID that is not enrolled yet the token " +
			"podium agent-token derives for it; an agent enrolled earlier must keep sending its existing token, " +
			"so fix its token transport: serve discovery over TLS so the SDK sends it, " +
			"or call allow_insecure_token_transport() in the SDK"
	)
	for _, transport := range []struct {
		name string
		sync func(logger zerolog.Logger, svc *agent.Service, hub *event.Hub) func() bool // reports a refused sync
	}{
		{"http", func(logger zerolog.Logger, svc *agent.Service, hub *event.Hub) func() bool {
			h := NewHTTPDiscovery(logger, svc, hub, discoveryLimiter(), nil, true)
			body := `{"id":"agent-a","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
			return func() bool {
				code := syncHTTP(h, body, "").Code
				return code == http.StatusUnauthorized || code == http.StatusTooManyRequests
			}
		}},
		{"grpc", func(logger zerolog.Logger, svc *agent.Service, hub *event.Hub) func() bool {
			g := NewGRPCDiscovery(logger, svc, hub, discoveryLimiter(), true)
			return func() bool {
				_, err := g.Sync(context.Background(), &discoverv1.SyncRequest{Id: "agent-a", Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
				code := grpcstatus.Code(err)
				return code == codes.Unauthenticated || code == codes.ResourceExhausted
			}
		}},
	} {
		for _, tc := range []struct {
			name    string
			opts    []agent.Option
			warning string
		}{
			{"no secret", nil, warning},
			{"secret", []agent.Option{agent.WithEnrollSecret(secret)}, warningWithSecret},
		} {
			t.Run(transport.name+"/"+tc.name, func(t *testing.T) {
				var logs bytes.Buffer
				svc := agent.New(inmemory.New(), zerolog.Nop(), tc.opts...)
				hub := event.NewHub(zerolog.Nop())
				defer hub.Close()
				sync := transport.sync(zerolog.New(&logs), svc, hub)
				for i := range 8 {
					if !sync() {
						t.Fatalf("tokenless sync %d was not refused", i+1)
					}
				}
				assertNotRegistered(t, svc, "agent-a")
				warnings := 0
				for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
					var entry struct {
						Level   string `json:"level"`
						AgentID string `json:"agent_id"`
						Message string `json:"message"`
					}
					if json.Unmarshal(line, &entry) == nil && entry.Level == "warn" && entry.Message == tc.warning && entry.AgentID == "agent-a" {
						warnings++
					}
				}
				if warnings != 5 {
					t.Fatalf("%d WARN lines %q for eight tokenless syncs, want 5 (rate_attempts):\n%s", warnings, tc.warning, logs.String())
				}
			})
		}
	}
}

// With an enrollment secret, an unknown agent ID that presents a token other
// than the one derived for it is logged at WARN with the agent ID and the
// client IP, never the token. Only attempts the limiter admits log, so a pair
// logs at most rate_attempts lines per window. A known agent with a wrong token
// and a sync without a token are no refused enrollment.
func TestRefusedEnrollmentWarns(t *testing.T) {
	const (
		secret  = "0123456789abcdef0123456789abcdef"
		token1  = "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058" // HMAC-SHA256(secret, "agent-1"), hex
		wrong   = "not-the-derived-token-of-this-agent"
		warning = "enrollment refused: an unknown agent ID presented a token that is not the one derived for it from the enrollment secret; give the agent the token podium agent-token prints for its ID"
	)
	for _, transport := range []struct {
		name string
		ip   string
		sync func(logger zerolog.Logger, svc *agent.Service, hub *event.Hub) func(id, token string) syncOutcome
	}{
		{"http", "192.0.2.1", func(logger zerolog.Logger, svc *agent.Service, hub *event.Hub) func(string, string) syncOutcome {
			h := NewHTTPDiscovery(logger, svc, hub, discoveryLimiter(), nil, true)
			return func(id, token string) syncOutcome {
				body := `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
				switch code := syncHTTP(h, body, token).Code; code {
				case http.StatusOK:
					return synced
				case http.StatusUnauthorized:
					return unauthenticated
				case http.StatusTooManyRequests:
					return refused
				default:
					return syncOutcome(fmt.Sprintf("HTTP %d", code))
				}
			}
		}},
		{"grpc", "203.0.113.9", func(logger zerolog.Logger, svc *agent.Service, hub *event.Hub) func(string, string) syncOutcome {
			g := NewGRPCDiscovery(logger, svc, hub, discoveryLimiter(), true)
			return func(id, token string) syncOutcome {
				ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 40000}})
				if token != "" {
					ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
				}
				_, err := g.Sync(ctx, &discoverv1.SyncRequest{Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
				switch grpcstatus.Code(err) {
				case codes.OK:
					return synced
				case codes.Unauthenticated:
					return unauthenticated
				case codes.ResourceExhausted:
					return refused
				default:
					return syncOutcome(err.Error())
				}
			}
		}},
	} {
		t.Run(transport.name, func(t *testing.T) {
			var logs bytes.Buffer
			svc := agent.New(inmemory.New(), zerolog.Nop(), agent.WithEnrollSecret(secret))
			hub := event.NewHub(zerolog.Nop())
			defer hub.Close()
			sync := transport.sync(zerolog.New(&logs), svc, hub)

			for i := range 8 {
				if got := sync("agent-2", wrong); got != unauthenticated && got != refused {
					t.Fatalf("agent-2 with a wrong token, attempt %d: %s", i+1, got)
				}
			}
			// No refused enrollment: a sync without a token, and a known agent
			// with a wrong token.
			if got := sync("agent-3", ""); got != unauthenticated {
				t.Fatalf("agent-3 without a token: %s, want unauthenticated", got)
			}
			if got := sync("agent-1", token1); got != synced {
				t.Fatalf("agent-1 with its derived token: %s, want synced", got)
			}
			if got := sync("agent-1", wrong); got != unauthenticated {
				t.Fatalf("enrolled agent-1 with a wrong token: %s, want unauthenticated", got)
			}

			if strings.Contains(logs.String(), wrong) {
				t.Fatalf("the log holds the presented token:\n%s", logs.String())
			}
			warnings := 0
			for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
				var entry struct {
					Level    string `json:"level"`
					AgentID  string `json:"agent_id"`
					ClientIP string `json:"client_ip"`
					Message  string `json:"message"`
				}
				if json.Unmarshal(line, &entry) != nil || entry.Message != warning {
					continue
				}
				if entry.Level != "warn" || entry.AgentID != "agent-2" || entry.ClientIP != transport.ip {
					t.Fatalf("refused enrollment logged as %+v, want WARN for agent-2 from %s", entry, transport.ip)
				}
				warnings++
			}
			if warnings != 5 {
				t.Fatalf("%d WARN lines for eight refused enrollments, want 5 (rate_attempts):\n%s", warnings, logs.String())
			}
		})
	}
}

// With an enrollment secret an unknown agent ID enrolls only with the token
// derived from it for that ID. Any other token is refused (HTTP 401, gRPC
// Unauthenticated) and stores nothing, and each refusal counts as a failure of
// the (client IP, agent ID) pair in the discovery limiter. Once enrolled, the
// agent syncs with that token.
func TestEnrollRequiresDerivedTokenWhenSecretSet(t *testing.T) {
	const (
		secret = "0123456789abcdef0123456789abcdef"
		token1 = "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058" // HMAC-SHA256(secret, "agent-1"), hex
		token2 = "14202fea7ea2cc313572d5d8e8c424489c954421b0f6160ce9e07d27340644fc" // the same for "agent-2"
	)
	for _, tc := range []struct {
		name string
		sync func(svc *agent.Service, hub *event.Hub) func(id, token string) syncOutcome
	}{
		{"http", func(svc *agent.Service, hub *event.Hub) func(string, string) syncOutcome {
			h := NewHTTPDiscovery(zerolog.Nop(), svc, hub, discoveryLimiter(), nil, true)
			return func(id, token string) syncOutcome {
				body := `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
				switch code := syncHTTP(h, body, token).Code; code {
				case http.StatusOK:
					return synced
				case http.StatusUnauthorized:
					return unauthenticated
				case http.StatusTooManyRequests:
					return refused
				default:
					return syncOutcome(fmt.Sprintf("HTTP %d", code))
				}
			}
		}},
		{"grpc", func(svc *agent.Service, hub *event.Hub) func(string, string) syncOutcome {
			g := NewGRPCDiscovery(zerolog.Nop(), svc, hub, discoveryLimiter(), true)
			return func(id, token string) syncOutcome {
				ctx := context.Background()
				if token != "" {
					ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
				}
				_, err := g.Sync(ctx, &discoverv1.SyncRequest{Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
				switch grpcstatus.Code(err) {
				case codes.OK:
					return synced
				case codes.Unauthenticated:
					return unauthenticated
				case codes.ResourceExhausted:
					return refused
				default:
					return syncOutcome(err.Error())
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc := agent.New(inmemory.New(), zerolog.Nop(), agent.WithEnrollSecret(secret))
			hub := event.NewHub(zerolog.Nop())
			defer hub.Close()
			sync := tc.sync(svc, hub)

			// No token, a made-up one and another agent's derived token.
			for _, token := range []string{"", "made-up-token", token2} {
				if got := sync("agent-1", token); got != unauthenticated {
					t.Fatalf("agent-1 with token %q: %s, want unauthenticated", token, got)
				}
				assertNotRegistered(t, svc, "agent-1")
			}

			if got := sync("agent-1", token1); got != synced {
				t.Fatalf("agent-1 with its derived token: %s, want synced", got)
			}
			if stored, err := svc.AgentToken(ctx, "agent-1"); err != nil || stored != token1 {
				t.Fatalf("stored token of agent-1: %q %v, want its derived token", stored, err)
			}
			if got := sync("agent-1", token1); got != synced {
				t.Fatalf("enrolled agent-1 syncing again: %s, want synced", got)
			}

			// Five refusals block the pair, even for the token that would enroll.
			for i := range 5 {
				if got := sync("agent-2", "made-up-token"); got != unauthenticated {
					t.Fatalf("agent-2 bad token %d: %s, want unauthenticated", i+1, got)
				}
			}
			if got := sync("agent-2", token2); got != refused {
				t.Fatalf("agent-2 after five refusals: %s, want refused", got)
			}
			assertNotRegistered(t, svc, "agent-2")
		})
	}
}

// Lifecycle deletes an agent's row but keeps its credential, so with an
// enrollment secret set an agent that lifecycle deleted is still a known ID: it
// syncs again with its stored token, here one it enrolled before the secret was
// set, and comes back. The token derived from the secret is refused for it: a
// known ID is checked against its stored token, not derived again.
func TestDeletedAgentSyncsAgainWithItsStoredToken(t *testing.T) {
	const (
		secret  = "0123456789abcdef0123456789abcdef"
		derived = "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058" // HMAC-SHA256(secret, "agent-1"), hex
		stored  = "token-enrolled-before-the-secret"
	)
	for _, transport := range []struct {
		name string
		sync func(svc *agent.Service, hub *event.Hub) func(token string) syncOutcome
	}{
		{"http", func(svc *agent.Service, hub *event.Hub) func(string) syncOutcome {
			h := NewHTTPDiscovery(zerolog.Nop(), svc, hub, discoveryLimiter(), nil, true)
			body := `{"id":"agent-1","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
			return func(token string) syncOutcome {
				switch code := syncHTTP(h, body, token).Code; code {
				case http.StatusOK:
					return synced
				case http.StatusUnauthorized:
					return unauthenticated
				default:
					return syncOutcome(fmt.Sprintf("HTTP %d", code))
				}
			}
		}},
		{"grpc", func(svc *agent.Service, hub *event.Hub) func(string) syncOutcome {
			g := NewGRPCDiscovery(zerolog.Nop(), svc, hub, discoveryLimiter(), true)
			return func(token string) syncOutcome {
				ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
				_, err := g.Sync(ctx, &discoverv1.SyncRequest{Id: "agent-1", Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1})
				switch grpcstatus.Code(err) {
				case codes.OK:
					return synced
				case codes.Unauthenticated:
					return unauthenticated
				default:
					return syncOutcome(err.Error())
				}
			}
		}},
	} {
		t.Run(transport.name, func(t *testing.T) {
			ctx := context.Background()
			store := inmemory.New()
			svc := agent.New(store, zerolog.Nop(), agent.WithEnrollSecret(secret))
			hub := event.NewHub(zerolog.Nop())
			defer hub.Close()
			cred, err := model.NewAgentCredential("agent-1", stored)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.UpsertAgentCredential(ctx, cred); err != nil {
				t.Fatal(err)
			}
			ag, err := model.NewAgent("agent-1", "agent-1", "http://10.0.0.5:8085")
			if err != nil {
				t.Fatal(err)
			}
			if err = store.UpsertAgent(ctx, ag); err != nil {
				t.Fatal(err)
			}
			if err = store.DeleteAgent(ctx, "agent-1"); err != nil { // what lifecycle deletion does
				t.Fatal(err)
			}
			sync := transport.sync(svc, hub)

			if got := sync(derived); got != unauthenticated {
				t.Fatalf("deleted agent-1 with the derived token: %s, want unauthenticated", got)
			}
			if got := sync(stored); got != synced {
				t.Fatalf("deleted agent-1 with its stored token: %s, want synced", got)
			}
			if _, err = svc.Get(ctx, "agent-1"); err != nil {
				t.Fatalf("agent-1 did not come back: %v", err)
			}
			if token, err := svc.AgentToken(ctx, "agent-1"); err != nil || token != stored {
				t.Fatalf("stored token of agent-1: %q %v, want the one it enrolled with", token, err)
			}
		})
	}
}
