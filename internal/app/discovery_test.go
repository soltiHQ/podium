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
	"google.golang.org/grpc/test/bufconn"

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

// serveDiscovery serves srv on an in-memory listener and returns a client of it.
func serveDiscovery(t *testing.T, srv *grpc.Server) discoverv1.DiscoverServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///discovery",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return discoverv1.NewDiscoverServiceClient(conn)
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
	ctx := context.Background()
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()

	t.Run("http", func(t *testing.T) {
		h := buildDiscoveryHandler(logger, svc, hub, standalone.NewLeadership(), 0, kit.NewLimiter(auth.Config{}), nil, true, false)
		sync := func(id, token string) int {
			body := `{"id":"` + id + `","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`
			r := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w.Code
		}
		for _, id := range []string{"http-a", "http-b"} {
			if code := sync(id, "token-"+id); code != http.StatusOK {
				t.Fatalf("enroll %s: HTTP %d", id, code)
			}
		}
		for i := range 5 {
			if code := sync("http-a", "stale-token"); code != http.StatusUnauthorized {
				t.Fatalf("bad token %d: HTTP %d, want 401", i+1, code)
			}
		}
		if code := sync("http-b", "token-http-b"); code != http.StatusOK {
			t.Fatalf("http-b behind the same IP: HTTP %d, want 200", code)
		}
		if code := sync("http-a", "token-http-a"); code != http.StatusTooManyRequests {
			t.Fatalf("http-a after five failures: HTTP %d, want 429", code)
		}
	})

	t.Run("grpc", func(t *testing.T) {
		client := serveDiscovery(t, buildGRPCServer(logger, svc, hub, standalone.NewLeadership(), kit.NewLimiter(auth.Config{}), nil, true))
		sync := func(id, token string) error {
			_, err := client.Sync(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), &discoverv1.SyncRequest{
				Id: id, Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1,
			})
			return err
		}
		for _, id := range []string{"grpc-a", "grpc-b"} {
			if err := sync(id, "token-"+id); err != nil {
				t.Fatalf("enroll %s: %v", id, err)
			}
		}
		for i := range 5 {
			if err := sync("grpc-a", "stale-token"); status.Code(err) != codes.Unauthenticated {
				t.Fatalf("bad token %d: %v, want Unauthenticated", i+1, err)
			}
		}
		if err := sync("grpc-b", "token-grpc-b"); err != nil {
			t.Fatalf("grpc-b behind the same peer: %v", err)
		}
		if err := sync("grpc-a", "token-grpc-a"); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("grpc-a after five failures: %v, want ResourceExhausted", err)
		}
	})
}
