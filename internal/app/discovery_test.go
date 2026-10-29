package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
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

// Discovery messages over the HTTP sync body limit never reach the handler.
func TestGRPCDiscoveryRejectsOversizedMessage(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	svc := agent.New(inmemory.New(), logger)
	hub := event.NewHub(logger)
	defer hub.Close()
	lis := bufconn.Listen(1 << 20)
	srv := buildGRPCServer(logger, svc, hub, standalone.NewLeadership(), nil, nil, false)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	conn, err := grpc.NewClient("passthrough:///discovery",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := discoverv1.NewDiscoverServiceClient(conn)
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
