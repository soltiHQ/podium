package interceptor

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
)

// leaderStub is a leadership whose leader a test can move.
type leaderStub struct {
	mu      sync.Mutex
	leading bool
	leader  string // the raft address of the leader, "" while none is known
}

func (l *leaderStub) AmLeader() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leading
}

func (l *leaderStub) CurrentLeader() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leader
}

func (*leaderStub) WhenLeader(ctx context.Context, _ func(context.Context) error) error {
	<-ctx.Done()
	return nil
}

func (l *leaderStub) moveTo(leader string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leader = leader
}

// served is a Sync a discovery served: the metadata and the deadline it got.
type served struct {
	md          metadata.MD
	deadline    time.Time
	hasDeadline bool
}

// discovery is a DiscoverService that records the syncs it serves and answers
// them with answer, or with success when answer is nil.
type discovery struct {
	discoverv1.UnimplementedDiscoverServiceServer
	calls  chan served
	answer error
}

func (d *discovery) Sync(ctx context.Context, _ *discoverv1.SyncRequest) (*discoverv1.SyncResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	deadline, ok := ctx.Deadline()
	d.calls <- served{md: md.Copy(), deadline: deadline, hasDeadline: ok}
	if d.answer != nil {
		return nil, d.answer
	}
	return &discoverv1.SyncResponse{Success: true}, nil
}

// countingListener counts the connections it accepted and those of them closed
// since.
type countingListener struct {
	net.Listener
	accepted, closed atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	return &countedConn{Conn: conn, closed: &l.closed}, nil
}

type countedConn struct {
	net.Conn
	once   sync.Once
	closed *atomic.Int32
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.closed.Add(1) })
	return c.Conn.Close()
}

// node is a gRPC discovery server on a loopback port.
type node struct {
	port      int
	discovery *discovery
	listener  *countingListener
}

// serve serves a discovery that answers with answer behind the interceptors,
// after UnaryRequestID as in the server, and returns its node.
func serve(t *testing.T, answer error, interceptors ...grpc.UnaryServerInterceptor) *node {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &node{
		port:      lis.Addr().(*net.TCPAddr).Port,
		discovery: &discovery{calls: make(chan served, 16), answer: answer},
		listener:  &countingListener{Listener: lis},
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(append([]grpc.UnaryServerInterceptor{UnaryRequestID()}, interceptors...)...))
	discoverv1.RegisterDiscoverServiceServer(srv, n.discovery)
	go func() { _ = srv.Serve(n.listener) }()
	t.Cleanup(srv.Stop)
	return n
}

// client dials n.
func (n *node) client(t *testing.T) discoverv1.DiscoverServiceClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///127.0.0.1:"+strconv.Itoa(n.port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return discoverv1.NewDiscoverServiceClient(conn)
}

// syncForwarder forwards Sync, as the composition root does.
func syncForwarder(t *testing.T) *Forwarder {
	t.Helper()
	f := NewForwarder(false, nil, map[string]ForwardFunc{
		discoverv1.DiscoverService_Sync_FullMethodName: func(ctx context.Context, conn grpc.ClientConnInterface, req any, opts ...grpc.CallOption) (any, error) {
			return discoverv1.NewDiscoverServiceClient(conn).Sync(ctx, req.(*discoverv1.SyncRequest), opts...)
		},
	})
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// followerOf serves a follower of leadership that forwards with forwarder to
// port, and fails the test when it serves a write itself.
func followerOf(t *testing.T, leadership *leaderStub, port int, forwarder *Forwarder) *node {
	t.Helper()
	n := serve(t, nil, UnaryLeader(leadership, LeaderOptions{Port: port, Forwarder: forwarder}))
	t.Cleanup(func() {
		if len(n.discovery.calls) > 0 && !leadership.AmLeader() {
			t.Error("the follower served a write itself")
		}
	})
	return n
}

// next is the next call n served, or a failure after a second.
func (n *node) next(t *testing.T) served {
	t.Helper()
	select {
	case s := <-n.discovery.calls:
		return s
	case <-time.After(time.Second):
		t.Fatal("the node served no call")
		return served{}
	}
}

func syncRequest() *discoverv1.SyncRequest {
	return &discoverv1.SyncRequest{Id: "agent-1", Endpoint: "10.0.0.5:50051", EndpointType: discoverv1.EndpointType_ENDPOINT_TYPE_GRPC, ApiVersion: 1}
}

// A follower forwards a write to the leader with authorization unchanged, its
// request ID, x-forwarded-for with the caller's peer appended to what the call
// carried, and the mark of a forwarded call, and nothing else of the call's
// metadata. The leader's answer, an error included, is the follower's.
func TestUnaryLeaderForwardsAWriteWithItsMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer error // the leader's
	}{
		{"the leader's success", nil},
		{"the leader's error", status.Error(codes.Unauthenticated, "agent authentication failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leader := serve(t, tc.answer)
			follower := followerOf(t, &leaderStub{leader: "127.0.0.1:7000"}, leader.port, syncForwarder(t))
			ctx := metadata.AppendToOutgoingContext(context.Background(),
				"authorization", "Bearer agent-token",
				"x-forwarded-for", "198.51.100.7",
				"x-request-id", "request-1",
				"x-other", "not forwarded",
			)
			resp, err := follower.client(t).Sync(ctx, syncRequest())
			if tc.answer == nil && (err != nil || !resp.GetSuccess()) {
				t.Fatalf("Sync through the follower: %v %v, want the leader's success", resp, err)
			}
			if tc.answer != nil && (status.Code(err) != codes.Unauthenticated || status.Convert(err).Message() != "agent authentication failed") {
				t.Fatalf("Sync through the follower: %v, want the leader's %v", err, tc.answer)
			}
			md := leader.next(t).md
			for key, want := range map[string]string{
				"authorization":   "Bearer agent-token",
				"x-forwarded-for": "198.51.100.7, 127.0.0.1",
				"x-request-id":    "request-1",
				ForwardedKey:      "1",
			} {
				if got := md.Get(key); len(got) != 1 || got[0] != want {
					t.Errorf("the leader got %s %q, want %q", key, got, want)
				}
			}
			if got := md.Get("x-other"); len(got) != 0 {
				t.Errorf("the leader got x-other %q, which the follower must not forward", got)
			}
		})
	}
}

// A forwarded call has the caller's deadline, and none when the caller set none:
// the follower adds no deadline of its own.
func TestForwardedCallKeepsTheCallersDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration // the caller's, 0 for none
	}{
		{"a caller with a deadline", 7 * time.Second},
		{"a caller without one", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leader := serve(t, nil)
			client := followerOf(t, &leaderStub{leader: "127.0.0.1:7000"}, leader.port, syncForwarder(t)).client(t)
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			if _, err := client.Sync(ctx, syncRequest()); err != nil {
				t.Fatal(err)
			}
			got := leader.next(t)
			want, has := ctx.Deadline()
			if got.hasDeadline != has {
				t.Fatalf("the leader's call has a deadline: %v, want %v", got.hasDeadline, has)
			}
			// gRPC sends the time left, so the leader's deadline is the caller's
			// give or take the time the call travelled.
			if has && (got.deadline.Before(want.Add(-time.Second)) || got.deadline.After(want.Add(time.Second))) {
				t.Fatalf("the leader's deadline is %s, want the caller's %s", got.deadline, want)
			}
		})
	}
}

// waitConns polls until the listener of n has accepted and closed the given
// numbers of connections, and fails the test when it has not within 5s.
func waitConns(t *testing.T, n *node, accepted, closed int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for n.listener.accepted.Load() != accepted || n.listener.closed.Load() != closed {
		if time.Now().After(deadline) {
			t.Fatalf("the leader accepted %d connections and saw %d close, want %d and %d", n.listener.accepted.Load(), n.listener.closed.Load(), accepted, closed)
		}
		<-tick.C
	}
}

// A follower forwards over one connection for as long as the leader keeps its
// address, opens another when the leader moves and closes the former leader's,
// and closes its last one on Close: a forward after that fails as one that
// cannot reach the leader.
func TestForwarderKeepsOneConnectionPerLeader(t *testing.T) {
	leader := serve(t, nil) // the leader, known as 127.0.0.1 first, and then as localhost
	leadership := &leaderStub{leader: "127.0.0.1:7000"}
	forwarder := syncForwarder(t)
	client := followerOf(t, leadership, leader.port, forwarder).client(t)
	ctx := context.Background()
	sync := func() error {
		_, err := client.Sync(ctx, syncRequest())
		return err
	}
	for range 3 {
		if err := sync(); err != nil {
			t.Fatal(err)
		}
		leader.next(t)
	}
	waitConns(t, leader, 1, 0)

	leadership.moveTo("localhost:7000")
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	leader.next(t)
	waitConns(t, leader, 2, 1)

	if err := forwarder.Close(); err != nil {
		t.Fatal(err)
	}
	waitConns(t, leader, 2, 2)
	if err := sync(); status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "leader unreachable" {
		t.Fatalf("Sync after Close: %v, want Unavailable leader unreachable", err)
	}
	waitConns(t, leader, 2, 2)
}

// A follower answers a write it does not get an answer to from the leader with
// Unavailable, and the x-leader hint whenever it can derive the leader's gRPC
// address: the host of the leader's raft address with the port of its own gRPC
// listener. A write it forwards and the leader never answers is "leader
// unreachable"; any other is "not the leader", a call another node forwarded
// included, which goes no further. Reads, and writes on the leader, are served
// where they arrive.
func TestUnaryLeaderAnswersWithTheLeadersGRPCAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := l.Addr().(*net.TCPAddr).Port // nothing listens there once l is closed
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	leader := serve(t, nil) // reached by no case: it fails the test if it serves one
	t.Cleanup(func() {
		if len(leader.discovery.calls) > 0 {
			t.Error("a call reached the leader")
		}
	})
	for _, tc := range []struct {
		name       string
		leadership *leaderStub
		port       int
		forward    bool        // the follower has a forwarder
		isWrite    bool        // Sync is a write
		incoming   metadata.MD // the metadata of the call
		code       codes.Code
		message    string
		hint       string // x-leader, "" for none
	}{
		{"an unreachable leader", &leaderStub{leader: "127.0.0.1:7000"}, unreachable, true, true, nil, codes.Unavailable, "leader unreachable", "127.0.0.1:" + strconv.Itoa(unreachable)},
		{"a call another node forwarded", &leaderStub{leader: "127.0.0.1:7000"}, leader.port, true, true, metadata.Pairs(ForwardedKey, "1"), codes.Unavailable, "not the leader", "127.0.0.1:" + strconv.Itoa(leader.port)},
		{"no leader known", &leaderStub{}, leader.port, true, true, nil, codes.Unavailable, "not the leader", ""},
		{"no port known", &leaderStub{leader: "127.0.0.1:7000"}, 0, true, true, nil, codes.Unavailable, "not the leader", ""},
		{"a write it does not forward", &leaderStub{leader: "10.0.0.1:7000"}, 50051, false, true, nil, codes.Unavailable, "not the leader", "10.0.0.1:50051"},
		{"an IPv6 leader", &leaderStub{leader: "[fd00::1]:7000"}, 50051, false, true, nil, codes.Unavailable, "not the leader", "[fd00::1]:50051"},
		{"a leader known by its host name", &leaderStub{leader: "podium-0.podium:7000"}, 50051, false, true, nil, codes.Unavailable, "not the leader", "podium-0.podium:50051"},
		{"a read", &leaderStub{leader: "127.0.0.1:7000"}, leader.port, true, false, nil, codes.OK, "", ""},
		{"a write on the leader", &leaderStub{leading: true}, leader.port, true, true, nil, codes.OK, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var forwarder *Forwarder
			if tc.forward {
				forwarder = syncForwarder(t)
			}
			opts := LeaderOptions{Port: tc.port, Forwarder: forwarder, IsWrite: func(string) bool { return tc.isWrite }}
			n := serve(t, nil, UnaryLeader(tc.leadership, opts))
			ctx := context.Background()
			if tc.incoming != nil {
				ctx = metadata.NewOutgoingContext(ctx, tc.incoming)
			}
			var header metadata.MD
			_, err := n.client(t).Sync(ctx, syncRequest(), grpc.Header(&header))
			if status.Code(err) != tc.code || (err != nil && status.Convert(err).Message() != tc.message) {
				t.Fatalf("Sync: %v, want %v %q", err, tc.code, tc.message)
			}
			if got := header.Get(LeaderKey); (tc.hint == "" && len(got) != 0) || (tc.hint != "" && (len(got) != 1 || got[0] != tc.hint)) {
				t.Fatalf("x-leader %q, want %q", got, tc.hint)
			}
			if local := len(n.discovery.calls) == 1; local != (tc.code == codes.OK) {
				t.Fatalf("the node served the call itself: %v, want %v", local, tc.code == codes.OK)
			}
		})
	}
}

// headerStream is a server stream that keeps the header metadata set on it.
type headerStream struct {
	grpc.ServerStream
	header metadata.MD
}

func (s *headerStream) SetHeader(md metadata.MD) error {
	s.header = metadata.Join(s.header, md)
	return nil
}

func (*headerStream) Context() context.Context { return context.Background() }

// StreamLeader refuses a write stream on a follower with the x-leader hint of
// UnaryLeader, the leader's gRPC address, and with none when it cannot derive
// it.
func TestStreamLeaderAnswersWithTheLeadersGRPCAddress(t *testing.T) {
	for _, tc := range []struct {
		name       string
		leadership *leaderStub
		hint       string
	}{
		{"a known leader", &leaderStub{leader: "10.0.0.1:7000"}, "10.0.0.1:50051"},
		{"no leader known", &leaderStub{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := &headerStream{}
			interceptor := StreamLeader(tc.leadership, LeaderOptions{Port: 50051, IsWrite: func(string) bool { return true }})
			err := interceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: "/solti.test.v1.Test/Watch"}, func(any, grpc.ServerStream) error {
				t.Fatal("a follower served a write stream")
				return nil
			})
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("write stream on a follower: %v, want Unavailable", err)
			}
			if got := ss.header.Get(LeaderKey); (tc.hint == "" && len(got) != 0) || (tc.hint != "" && (len(got) != 1 || got[0] != tc.hint)) {
				t.Fatalf("x-leader %q, want %q", got, tc.hint)
			}
		})
	}
}
