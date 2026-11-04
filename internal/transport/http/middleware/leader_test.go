package middleware

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

func TestBuildTarget(t *testing.T) {
	tests := []struct {
		name       string
		leaderAddr string
		port       int
		tls        bool
		want       string
	}{
		{"ipv4 host:port → http", "10.0.0.1:7000", 8080, false, "http://10.0.0.1:8080"},
		{"ipv4 with tls → https", "10.0.0.1:7000", 8080, true, "https://10.0.0.1:8080"},
		{"ipv6 bracketed → rebracketed", "[::1]:7000", 8080, false, "http://[::1]:8080"},
		{"bare host (no port)", "leader-host", 8082, false, "http://leader-host:8082"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := buildTarget(tt.leaderAddr, tt.port, tt.tls)
			if err != nil {
				t.Fatalf("buildTarget error: %v", err)
			}
			if got := u.String(); got != tt.want {
				t.Fatalf("buildTarget(%q, %d, %v) = %q, want %q", tt.leaderAddr, tt.port, tt.tls, got, tt.want)
			}
		})
	}
}

// The forward target needs both a leader address and a port to forward to.
func TestBuildTargetNeedsAnAddressAndAPort(t *testing.T) {
	for _, tt := range []struct {
		name, leaderAddr string
		port             int
	}{
		{"no leader address", "", 8080},
		{"no port", "10.0.0.1:7000", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if u, err := buildTarget(tt.leaderAddr, tt.port, false); err == nil {
				t.Fatalf("buildTarget(%q, %d) = %v, want an error", tt.leaderAddr, tt.port, u)
			}
		})
	}
}

// stubLeadership is a leadership in a fixed state.
type stubLeadership struct {
	leading bool
	leader  string // the raft address of the leader, if any
}

func (s stubLeadership) AmLeader() bool        { return s.leading }
func (s stubLeadership) CurrentLeader() string { return s.leader }
func (stubLeadership) WhenLeader(ctx context.Context, _ func(context.Context) error) error {
	<-ctx.Done()
	return nil
}

// A follower tells where the leader serves the listener: the host of the
// leader's raft address with its own port for the listener, the very address
// it forwards a write to. It tells nothing as the leader, or with no leader.
func TestLeaderAddress(t *testing.T) {
	tests := []struct {
		name       string
		leadership stubLeadership
		port       int
		want       string
	}{
		{"the leader itself", stubLeadership{leading: true, leader: "10.0.0.1:7000"}, 8080, ""},
		{"a follower", stubLeadership{leader: "10.0.0.1:7000"}, 8080, "10.0.0.1:8080"},
		{"an IPv6 leader", stubLeadership{leader: "[fd00::1]:7000"}, 8080, "[fd00::1]:8080"},
		{"a leader known by its host name", stubLeadership{leader: "podium-0.podium:7000"}, 8080, "podium-0.podium:8080"},
		{"no leader known", stubLeadership{}, 8080, ""},
		{"no port to serve on", stubLeadership{leader: "10.0.0.1:7000"}, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := leaderAddress(tt.leadership, tt.port)
			if got != tt.want {
				t.Fatalf("leaderAddress(%+v, %d) = %q, want %q", tt.leadership, tt.port, got, tt.want)
			}
			if tt.want == "" || tt.port == 0 {
				return
			}
			target, err := buildTarget(tt.leadership.leader, tt.port, false)
			if err != nil || target.Host != got {
				t.Fatalf("a write is forwarded to %v (%v), not to %q", target, err, got)
			}
		})
	}
}

// X-Leader has one meaning, the leader's HTTP address (host:port), so the answers
// the Leader middleware gives for itself carry that or no header: never the
// leader's raft address, which they used to send. With no HTTP port to forward to
// it has no HTTP address to give and sends none; a leader it cannot reach is 502
// with the address it tried.
func TestLeaderAnswersCarryTheLeadersHTTPAddressOrNone(t *testing.T) {
	// A port nothing listens on, for a leader that cannot be reached.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		leadership stubLeadership
		port       int
		status     int
		leader     string // X-Leader
	}{
		{"no leader known", stubLeadership{}, 8080, http.StatusServiceUnavailable, ""},
		{"no HTTP port to forward to", stubLeadership{leader: "10.0.0.1:7000"}, 0, http.StatusServiceUnavailable, ""},
		{"a leader that cannot be reached", stubLeadership{leader: "127.0.0.1:7000"}, unreachable, http.StatusBadGateway, net.JoinHostPort("127.0.0.1", strconv.Itoa(unreachable))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("a write reached the handler of a follower") })
			w := httptest.NewRecorder()
			Leader(tt.leadership, LeaderOptions{ForwardPort: tt.port})(next).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if _, set := w.Header()[response.LeaderHeader]; set != (tt.leader != "") || w.Header().Get(response.LeaderHeader) != tt.leader {
				t.Fatalf("%s = %q, want %q", response.LeaderHeader, w.Header().Get(response.LeaderHeader), tt.leader)
			}
		})
	}
}

// A request that passes through carries the leader's address in its context, for
// the answer to a write that reaches a follower anyway.
func TestLeaderPassesTheLeaderAddressOn(t *testing.T) {
	tests := []struct {
		name       string
		leadership stubLeadership
		method     string
		want       string
	}{
		{"a read on a follower", stubLeadership{leader: "10.0.0.1:7000"}, http.MethodGet, "10.0.0.1:8080"},
		{"a read on a follower with no leader", stubLeadership{}, http.MethodGet, ""},
		{"a write on the leader", stubLeadership{leading: true, leader: "10.0.0.1:7000"}, http.MethodPost, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reached bool
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				reached = true
				if got := httpctx.Leader(r.Context()); got != tt.want {
					t.Errorf("httpctx.Leader() = %q, want %q", got, tt.want)
				}
			})
			Leader(tt.leadership, LeaderOptions{ForwardPort: 8080})(next).
				ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tt.method, "/", nil))
			if !reached {
				t.Fatal("the request did not reach the handler")
			}
		})
	}
}
