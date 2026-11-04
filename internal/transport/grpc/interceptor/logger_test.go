package interceptor

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// logSink collects what a logger writes; the server's goroutines write while the
// test reads it.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// levels returns the level of every line logged with message.
func (s *logSink) levels(t *testing.T, message string) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(s.buf.String()), "\n") {
		var entry struct{ Level, Message string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Message == message {
			levels = append(levels, entry.Level)
		}
	}
	return levels
}

// UnaryLogger logs a call at INFO, and one that fails at ERROR, except the
// "not the leader" answer with the x-leader hint, which it logs at WARN: a write
// that reached a follower is a routing condition, not a failure of the node, as
// the HTTP access log has it for a 503 with X-Leader. A follower passes on the
// routing answer of a node it forwarded to, that node's hint included, and logs
// it at WARN too. Every other failure stays an ERROR: an Unavailable without the
// hint, and "leader unreachable", which keeps its hint, as HTTP's 502 does.
func TestUnaryLoggerLogsRoutingAnswersAtWarn(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := l.Addr().(*net.TCPAddr).Port // nothing listens there once l is closed
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		node  func(t *testing.T, logged grpc.UnaryServerInterceptor) *node // the node called, logging with logged
		code  codes.Code
		hint  string
		level string
	}{
		{"a write on a follower", func(t *testing.T, logged grpc.UnaryServerInterceptor) *node {
			return serve(t, nil, logged, UnaryLeader(&leaderStub{leader: "10.0.0.1:7000"}, LeaderOptions{Port: 50051}))
		}, codes.Unavailable, "10.0.0.1:50051", "warn"},
		{"a routing answer it forwards on", func(t *testing.T, logged grpc.UnaryServerInterceptor) *node {
			// A node that takes yet another for the leader, and refuses a forwarded call.
			other := serve(t, nil, UnaryLeader(&leaderStub{leader: "10.0.0.3:7000"}, LeaderOptions{Port: 50051, Forwarder: syncForwarder(t)}))
			return serve(t, nil, logged, UnaryLeader(&leaderStub{leader: "127.0.0.1:7000"}, LeaderOptions{Port: other.port, Forwarder: syncForwarder(t)}))
		}, codes.Unavailable, "10.0.0.3:50051", "warn"},
		{"a forward the leader never answers", func(t *testing.T, logged grpc.UnaryServerInterceptor) *node {
			return serve(t, nil, logged, UnaryLeader(&leaderStub{leader: "127.0.0.1:7000"}, LeaderOptions{Port: unreachable, Forwarder: syncForwarder(t)}))
		}, codes.Unavailable, "127.0.0.1:" + strconv.Itoa(unreachable), "error"},
		{"a write on a follower that knows no leader", func(t *testing.T, logged grpc.UnaryServerInterceptor) *node {
			return serve(t, nil, logged, UnaryLeader(&leaderStub{}, LeaderOptions{Port: 50051}))
		}, codes.Unavailable, "", "error"},
		{"an Unavailable of the handler", func(t *testing.T, logged grpc.UnaryServerInterceptor) *node {
			return serve(t, status.Error(codes.Unavailable, "unavailable"), logged, UnaryLeader(&leaderStub{leading: true}, LeaderOptions{Port: 50051}))
		}, codes.Unavailable, "", "error"},
		{"another error", func(t *testing.T, logged grpc.UnaryServerInterceptor) *node {
			return serve(t, status.Error(codes.Unauthenticated, "agent authentication failed"), logged, UnaryLeader(&leaderStub{leading: true}, LeaderOptions{Port: 50051}))
		}, codes.Unauthenticated, "", "error"},
		{"a success", func(t *testing.T, logged grpc.UnaryServerInterceptor) *node {
			return serve(t, nil, logged, UnaryLeader(&leaderStub{leading: true}, LeaderOptions{Port: 50051}))
		}, codes.OK, "", "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &logSink{}
			n := tc.node(t, UnaryLogger(zerolog.New(logs)))
			var header metadata.MD
			_, err := n.client(t).Sync(context.Background(), syncRequest(), grpc.Header(&header))
			if status.Code(err) != tc.code {
				t.Fatalf("Sync: %v, want %v", err, tc.code)
			}
			if got := header.Get(LeaderKey); (tc.hint == "" && len(got) != 0) || (tc.hint != "" && (len(got) != 1 || got[0] != tc.hint)) {
				t.Fatalf("x-leader %q, want %q", got, tc.hint)
			}
			if got := logs.levels(t, "grpc request"); len(got) != 1 || got[0] != tc.level {
				t.Fatalf("logged the call at %v, want once at %s", got, tc.level)
			}
		})
	}
}
