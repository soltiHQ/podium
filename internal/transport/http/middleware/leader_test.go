package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
		{"no port to serve on: the raft address", stubLeadership{leader: "10.0.0.1:7000"}, 0, "10.0.0.1:7000"},
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
