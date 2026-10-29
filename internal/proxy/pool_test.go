package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/enum"
)

// authRecordingServer reports the authorization metadata of every GetTask call.
type authRecordingServer struct {
	taskContractServer
	got chan<- string
}

func (s *authRecordingServer) GetTask(ctx context.Context, r *taskv1.GetTaskRequest) (*taskv1.GetTaskResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.got <- strings.Join(md.Get("authorization"), ",")
	return s.taskContractServer.GetTask(ctx, r)
}

func TestProxyDoesNotSendTokenWhenAuthDisabled(t *testing.T) {
	for _, tc := range []struct {
		name        string
		requireAuth bool
		want        string
	}{
		{"auth disabled", false, ""},
		{"auth required", true, "Bearer stored-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 1)
			httpAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Get("Authorization")
				_, _ = fmt.Fprint(w, `{"metadata":{"name":"task","uid":"u"}}`)
			}))
			defer httpAgent.Close()
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			grpcAgent := grpc.NewServer()
			taskv1.RegisterTaskServiceServer(grpcAgent, &authRecordingServer{got: got})
			go func() { _ = grpcAgent.Serve(lis) }()
			defer grpcAgent.Stop()
			pool := NewPool(nil, tc.requireAuth)
			defer func() { _ = pool.Close() }()
			for _, agent := range []struct {
				endpoint string
				kind     enum.EndpointType
			}{{httpAgent.URL, enum.EndpointHTTP}, {lis.Addr().String(), enum.EndpointGRPC}} {
				p, err := pool.Get(agent.endpoint, agent.kind, enum.APIVersionV1, "stored-token")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.GetTask(context.Background(), "task"); err != nil {
					t.Fatal(err)
				}
				if auth := <-got; auth != tc.want {
					t.Fatalf("%s agent received authorization %q, want %q", agent.kind, auth, tc.want)
				}
			}
		})
	}
}

func TestHTTPProxyDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"task","uid":"u"}}`)
	}))
	defer elsewhere.Close()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	defer agent.Close()
	pool := NewPool(nil, true)
	defer func() { _ = pool.Close() }()
	p, err := pool.Get(agent.URL, enum.EndpointHTTP, enum.APIVersionV1, "stored-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetTask(context.Background(), "task"); !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("redirect answer accepted: %v", err)
	}
	if followed.Load() {
		t.Fatal("proxy followed the agent's redirect")
	}
}

// Agents stored before endpoint validation existed must not get a client.
func TestPoolRejectsInvalidStoredEndpoint(t *testing.T) {
	pool := NewPool(nil, true)
	defer func() { _ = pool.Close() }()
	for _, tc := range []struct {
		endpoint string
		kind     enum.EndpointType
	}{
		{"http://[", enum.EndpointHTTP},
		{"http://169.254.169.254/latest/meta-data/#", enum.EndpointHTTP},
		{"unix:///run/agent.sock", enum.EndpointGRPC},
	} {
		if _, err := pool.Get(tc.endpoint, tc.kind, enum.APIVersionV1, ""); !errors.Is(err, ErrBadEndpointURL) || !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Get(%q, %s) = %v, want ErrBadEndpointURL wrapping ErrInvalidArgument", tc.endpoint, tc.kind, err)
		}
	}
}

func TestNormalizeGrpcTarget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare host:port unchanged", "localhost:50052", "localhost:50052"},
		{"ipv4 host:port unchanged", "10.0.0.1:50051", "10.0.0.1:50051"},
		{"ipv6 host:port unchanged", "[::1]:50051", "[::1]:50051"},
		{"strips http scheme", "http://localhost:50052", "localhost:50052"},
		{"strips https scheme", "https://agent.example.com:443", "agent.example.com:443"},
		{"strips trailing slash", "localhost:50052/", "localhost:50052"},
		{"strips scheme + slash", "http://localhost:50052/", "localhost:50052"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeGrpcTarget(tc.in)
			if got != tc.want {
				t.Errorf("normalizeGrpcTarget(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPoolGetAfterCloseReturnsError(t *testing.T) {
	p := NewPool(nil, false)
	_ = p.Close()
	if _, err := p.Get("localhost:9999", enum.EndpointGRPC, enum.APIVersionV1, ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed pool reopened: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
