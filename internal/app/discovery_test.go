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

func TestNewWarnsWhenAgentAuthDisabled(t *testing.T) {
	const warning = "agent authentication disabled: any host that reaches the discovery listeners can register agents"
	for _, tc := range []struct {
		name    string
		require bool
		want    int
	}{{"required", true, 0}, {"disabled", false, 1}} {
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
			warnings := 0
			for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
				var entry struct {
					Level   string `json:"level"`
					Message string `json:"message"`
				}
				if json.Unmarshal(line, &entry) == nil && entry.Message == warning && entry.Level == "warn" {
					warnings++
				}
			}
			if warnings != tc.want {
				t.Fatalf("agent auth warnings = %d, want %d", warnings, tc.want)
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
// buildDiscoveryListeners from the default config, always as the client 127.0.0.1.
type discoveryClients struct {
	http http.Handler
	grpc discoverv1.DiscoverServiceClient
}

func newDiscoveryClients(t *testing.T) discoveryClients {
	t.Helper()
	logger := zerolog.Nop()
	hub := event.NewHub(logger)
	t.Cleanup(hub.Close)
	h, srv := buildDiscoveryListeners(logger, agent.New(inmemory.New(), logger), hub, standalone.NewLeadership(), config.Default(), nil, nil)
	return discoveryClients{http: h, grpc: serveDiscovery(t, srv)}
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
