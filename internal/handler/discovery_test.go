package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// syncHTTP posts one proto-JSON heartbeat, with a bearer token when token is set.
func syncHTTP(h *HTTPDiscovery, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res := httptest.NewRecorder()
	h.Sync(res, req)
	return res
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
	h := NewHTTPDiscovery(logger, svc, hub, true)
	for _, tc := range []struct{ name, id, body string }{
		{"unsafe id", `x'"`, `{"id":"x'\"","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`},
		{"unparsable endpoint", "agent-1", `{"id":"agent-1","endpoint":"http://[","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`},
		{"endpoint query", "agent-1", `{"id":"agent-1","endpoint":"http://10.0.0.5:8085?x=1","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`},
		{"heartbeat over one hour", "agent-1", `{"id":"agent-1","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1,"heartbeatIntervalS":3601}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if res := syncHTTP(h, tc.body, "agent-token"); res.Code != http.StatusBadRequest {
				t.Fatalf("invalid agent sync: HTTP %d %s, want 400", res.Code, res.Body.String())
			}
			assertNotRegistered(t, svc, tc.id)
		})
	}
}

func TestGRPCDiscoveryRejectsInvalidAgent(t *testing.T) {
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	g := NewGRPCDiscovery(logger, svc, hub, true)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer agent-token"))
	tooMuchMetadata := make(map[string]string, 65)
	for i := range 65 {
		tooMuchMetadata[fmt.Sprintf("key-%d", i)] = "v"
	}
	for _, tc := range []struct {
		name string
		req  *discoverv1.SyncRequest
	}{
		{"unsafe id", &discoverv1.SyncRequest{Id: `x'"`, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1}},
		{"resolver scheme endpoint", &discoverv1.SyncRequest{Id: "agent-1", Endpoint: "unix:///run/agent.sock", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1}},
		{"too much metadata", &discoverv1.SyncRequest{Id: "agent-1", Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1, Metadata: tooMuchMetadata}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := g.Sync(ctx, tc.req); grpcstatus.Code(err) != codes.InvalidArgument {
				t.Fatalf("invalid agent sync: %v, want InvalidArgument", err)
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
	h := NewHTTPDiscovery(logger, svc, hub, requireAuth)
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
	g := NewGRPCDiscovery(logger, svc, hub, requireAuth)
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
	handler := NewHTTPDiscovery(logger, svc, hub, false)
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
	req = httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body+strings.Repeat(" ", maxSyncBodyBytes)))
	res = httptest.NewRecorder()
	handler.Sync(res, req)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize valid prefix accepted: %d", res.Code)
	}
}
