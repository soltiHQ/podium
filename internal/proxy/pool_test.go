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
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
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
				p, err := pool.Get("agent-1", agent.endpoint, agent.kind, enum.APIVersionV1, "stored-token")
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
	p, err := pool.Get("agent-1", agent.URL, enum.EndpointHTTP, enum.APIVersionV1, "stored-token")
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
		if _, err := pool.Get("agent-1", tc.endpoint, tc.kind, enum.APIVersionV1, ""); !errors.Is(err, ErrBadEndpointURL) || !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Get(%q, %s) = %v, want ErrBadEndpointURL wrapping ErrInvalidArgument", tc.endpoint, tc.kind, err)
		}
	}
}

// An advertised base path is joined to the task API path without "//".
func TestHTTPProxyJoinsBasePath(t *testing.T) {
	paths := make(chan string, 1)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			_, _ = fmt.Fprint(w, `{"metadata":{},"items":[]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"task","uid":"u"}}`)
	}))
	defer agent.Close()
	pool := NewPool(nil, true)
	defer func() { _ = pool.Close() }()
	for _, endpoint := range []string{agent.URL + "/agents/a1", agent.URL + "/agents/a1/"} {
		p, err := pool.Get("agent-1", endpoint, enum.EndpointHTTP, enum.APIVersionV1, "")
		if err != nil {
			t.Fatalf("Get(%q): %v", endpoint, err)
		}
		if _, err := p.GetTask(context.Background(), "task"); err != nil {
			t.Fatal(err)
		}
		if got := <-paths; got != "/agents/a1/apis/solti.io/v1/tasks/task" {
			t.Fatalf("%s: GetTask requested %q", endpoint, got)
		}
		if _, err := p.ListTasks(context.Background(), TaskFilter{}); err != nil {
			t.Fatal(err)
		}
		if got := <-paths; got != "/agents/a1/apis/solti.io/v1/tasks" {
			t.Fatalf("%s: ListTasks requested %q", endpoint, got)
		}
	}
}

// "unix:1" is the host "unix" on port 1. grpc-go's resolver registry would
// otherwise read it as the unix socket ./1.
func TestPoolDoesNotDialGRPCEndpointAsUnixSocket(t *testing.T) {
	t.Chdir(t.TempDir())
	lis, err := net.Listen("unix", "1")
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan string, 1)
	socketServer := grpc.NewServer()
	taskv1.RegisterTaskServiceServer(socketServer, &authRecordingServer{got: called})
	go func() { _ = socketServer.Serve(lis) }()
	defer socketServer.Stop()
	pool := NewPool(nil, true)
	defer func() { _ = pool.Close() }()
	p, err := pool.Get("agent-1", "unix:1", enum.EndpointGRPC, enum.APIVersionV1, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = p.GetTask(ctx, "task")
	select {
	case <-called:
		t.Fatal("gRPC endpoint unix:1 was dialed as the unix socket ./1")
	default:
	}
	if err == nil {
		t.Fatal("GetTask to the unresolvable host unix:1 succeeded")
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
	if _, err := p.Get("agent-1", "localhost:9999", enum.EndpointGRPC, enum.APIVersionV1, ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed pool reopened: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// grpcConns is the gRPC connections the pool holds, by agent.
func grpcConns(p *Pool) map[string]*grpc.ClientConn {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]*grpc.ClientConn, len(p.grpcConns))
	for agent, held := range p.grpcConns {
		out[agent] = held.conn
	}
	return out
}

// connTo gets the gRPC proxy of an agent from the pool, and returns its connection.
func connTo(t *testing.T, pool *Pool, agent, endpoint string) *grpc.ClientConn {
	t.Helper()
	p, err := pool.Get(agent, endpoint, enum.EndpointGRPC, enum.APIVersionV1, "")
	if err != nil {
		t.Fatal(err)
	}
	return p.(*grpcProxyV1).conn
}

// closed reports whether the pool closed the connection.
func closed(conn *grpc.ClientConn) bool { return conn.GetState() == connectivity.Shutdown }

// The pool holds one connection for an agent, to the endpoint it advertises now. The
// endpoints an agent advertised before are not kept: an agent that moves, or that
// advertises a new port at each restart, adds nothing.
func TestPoolClosesConnOnEndpointChange(t *testing.T) {
	pool := NewPool(nil, true)
	defer func() { _ = pool.Close() }()

	first := connTo(t, pool, "agent-1", "127.0.0.1:50051")
	if again := connTo(t, pool, "agent-1", "127.0.0.1:50051"); again != first || closed(first) {
		t.Fatal("the same agent at the same endpoint did not get its connection back")
	}
	other := connTo(t, pool, "agent-2", "127.0.0.1:50052")
	if other == first {
		t.Fatal("two agents share a connection")
	}

	moved := connTo(t, pool, "agent-1", "127.0.0.1:50053")

	if moved == first || !closed(first) {
		t.Fatalf("the agent moved: the old connection is %v, the new one is the old one: %v", first.GetState(), moved == first)
	}
	if closed(moved) || closed(other) {
		t.Fatal("the move closed a connection that is in use")
	}
	for i := range 5 {
		connTo(t, pool, "agent-1", fmt.Sprintf("127.0.0.1:%d", 50100+i))
	}
	if held := grpcConns(pool); len(held) != 2 {
		t.Fatalf("the pool holds %d connections for 2 agents", len(held))
	}
}

// An agent that advertises an HTTP endpoint after a gRPC one does not keep the
// connection to the old.
func TestPoolClosesConnWhenAgentMovesToHTTP(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer agent.Close()
	pool := NewPool(nil, true)
	defer func() { _ = pool.Close() }()
	conn := connTo(t, pool, "agent-1", "127.0.0.1:50051")
	other := connTo(t, pool, "agent-2", "127.0.0.1:50052")

	if _, err := pool.Get("agent-1", agent.URL, enum.EndpointHTTP, enum.APIVersionV1, ""); err != nil {
		t.Fatal(err)
	}

	if !closed(conn) || closed(other) {
		t.Fatalf("connection of the agent that moved: %v, of the other: %v", conn.GetState(), other.GetState())
	}
	if held := grpcConns(pool); len(held) != 1 {
		t.Fatalf("the pool holds %d connections", len(held))
	}
}

// Forget closes the connection of an agent that is gone or has a new credential. The
// pool keeps no token, Get takes it on every call, so the next call presents the
// token it is given.
func TestPoolForgetClosesConn(t *testing.T) {
	got := make(chan string, 1)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agent := grpc.NewServer()
	taskv1.RegisterTaskServiceServer(agent, &authRecordingServer{got: got})
	go func() { _ = agent.Serve(lis) }()
	defer agent.Stop()
	pool := NewPool(nil, true)
	defer func() { _ = pool.Close() }()
	call := func(token string) *grpc.ClientConn {
		t.Helper()
		p, err := pool.Get("agent-1", lis.Addr().String(), enum.EndpointGRPC, enum.APIVersionV1, token)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err = p.GetTask(ctx, "task"); err != nil {
			t.Fatal(err)
		}
		return p.(*grpcProxyV1).conn
	}
	other := connTo(t, pool, "agent-2", "127.0.0.1:50052")

	before := call("old-token")
	if seen := <-got; seen != "Bearer old-token" {
		t.Fatalf("the agent saw %q", seen)
	}

	pool.Forget("agent-1")

	if !closed(before) || closed(other) {
		t.Fatalf("Forget left the connection of the agent in state %v, or closed the other: %v", before.GetState(), other.GetState())
	}
	if held := grpcConns(pool); len(held) != 1 || held["agent-2"] != other {
		t.Fatalf("the pool holds %v after Forget, want the other agent's connection only", held)
	}
	after := call("new-token")
	if seen := <-got; seen != "Bearer new-token" {
		t.Fatalf("after Forget the agent saw %q", seen)
	}
	if after == before || closed(after) {
		t.Fatal("the next call after Forget did not dial afresh")
	}
}

// Forget ends a log stream open on the connection it closes, as any call in flight
// on it: the agent keeps the stream open, and the call has no deadline, so the
// stream ends because the client does.
func TestPoolForgetEndsALogStreamOnTheConnection(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agent := grpc.NewServer()
	taskv1.RegisterTaskServiceServer(agent, &chunkServer{lines: [][]byte{[]byte("first")}})
	go func() { _ = agent.Serve(lis) }()
	defer agent.Stop()
	pool := NewPool(nil, true)
	defer func() { _ = pool.Close() }()
	p, err := pool.Get("agent-1", lis.Addr().String(), enum.EndpointGRPC, enum.APIVersionV1, "token")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := p.(TaskLogStreamer).StreamTaskLogsForUID(ctx, "task", "uid")
	if err != nil {
		t.Fatal(err)
	}
	if ev, ok := <-stream; !ok || string(ev.GetChunk().GetLine()) != "first" {
		t.Fatalf("first event %v, open %v, want the first chunk", ev, ok)
	}

	pool.Forget("agent-1")

	select {
	case _, open := <-stream:
		if open {
			t.Fatal("the stream delivered an event after Forget")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stayed open 5s after Forget")
	}
}

// Forget has nothing to do for an agent the pool does not know, for one it forgot
// already, for an HTTP agent or on a pool that is closed.
func TestPoolForgetIsHarmlessWithoutAConn(t *testing.T) {
	pool := NewPool(nil, true)
	pool.Forget("unknown")
	conn := connTo(t, pool, "agent-1", "127.0.0.1:50051")
	pool.Forget("agent-1")
	pool.Forget("agent-1")
	if !closed(conn) {
		t.Fatal("Forget left the connection open")
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	pool.Forget("agent-1")
}

// Close releases the connections that Forget and a move have not.
func TestPoolCloseClosesWhatIsHeld(t *testing.T) {
	pool := NewPool(nil, true)
	conns := []*grpc.ClientConn{connTo(t, pool, "agent-1", "127.0.0.1:50051"), connTo(t, pool, "agent-2", "127.0.0.1:50052")}

	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}

	for i, conn := range conns {
		if !closed(conn) {
			t.Fatalf("connection %d is %v after Close", i, conn.GetState())
		}
	}
}
