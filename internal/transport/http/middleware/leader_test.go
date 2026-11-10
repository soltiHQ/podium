package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
			refused := 0
			Leader(tt.leadership, LeaderOptions{ForwardPort: tt.port, Refused: func() { refused++ }})(next).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if refused != 1 {
				t.Fatalf("Refused called %d times for the refusal, want once", refused)
			}
			if _, set := w.Header()[response.LeaderHeader]; set != (tt.leader != "") || w.Header().Get(response.LeaderHeader) != tt.leader {
				t.Fatalf("%s = %q, want %q", response.LeaderHeader, w.Header().Get(response.LeaderHeader), tt.leader)
			}
		})
	}
}

// hopNode is a node of TestLeaderForwardsAWriteOnce: the Leader middleware over a
// handler that serves a write, behind a count of the requests that reach it. A
// node reached more than three times answers 508, so that a write going round
// ends.
type hopNode struct {
	srv     *httptest.Server
	reached atomic.Int32
}

func newHopNode(t *testing.T) *hopNode {
	t.Helper()
	n := &hopNode{srv: httptest.NewUnstartedServer(nil)}
	t.Cleanup(n.srv.Close)
	return n
}

func (n *hopNode) port() int { return n.srv.Listener.Addr().(*net.TCPAddr).Port }

// start serves the Leader middleware of leadership, forwarding to port.
func (n *hopNode) start(leadership stubLeadership, port int) {
	served := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	leader := Leader(leadership, LeaderOptions{ForwardPort: port})(served)
	n.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.reached.Add(1) > 3 {
			w.WriteHeader(http.StatusLoopDetected)
			return
		}
		leader.ServeHTTP(w, r)
	})
	n.srv.Start()
}

// A follower marks a write it forwards, and never forwards a marked write
// again: it answers 503 with X-Leader, as a follower that cannot forward. So two
// followers that each take the other for the leader, as a stale view or
// replicas on mixed ports can, pass a write on once and then answer 503,
// instead of sending it round. A write carries the mark when it has the header,
// whatever its value. A client that sends the mark itself only keeps its own
// write from being forwarded, and the leader serves a marked write. A client
// that names the mark in its Connection header, which makes the reverse proxy
// drop that header as hop-by-hop, keeps no write from being marked.
func TestLeaderForwardsAWriteOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		leading    bool     // a, the node the client sends the write to, leads
		mark       []string // the values of the mark the client sends, nil for none
		connection string   // the client's Connection header, "" for none
		status     int      // the answer to the client
		reached    [2]int   // the requests that reached a and b
	}{
		{"two followers that take each other for the leader", false, nil, "", http.StatusServiceUnavailable, [2]int{1, 1}},
		{"a write the client marked", false, []string{"1"}, "", http.StatusServiceUnavailable, [2]int{1, 0}},
		{"a write the client marked with an empty value", false, []string{""}, "", http.StatusServiceUnavailable, [2]int{1, 0}},
		{"a marked write on the leader", true, []string{"1"}, "", http.StatusNoContent, [2]int{1, 0}},
		{"a Connection header that names the mark", false, nil, ForwardedHeader, http.StatusServiceUnavailable, [2]int{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newHopNode(t), newHopNode(t)
			// Each knows the leader by the raft host 127.0.0.1, and forwards to the
			// other's port: each one's view of the leader is the other.
			a.start(stubLeadership{leading: tc.leading, leader: "127.0.0.1:7000"}, b.port())
			b.start(stubLeadership{leader: "127.0.0.1:7000"}, a.port())

			r, err := http.NewRequest(http.MethodPost, a.srv.URL+"/api/v1/users", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range tc.mark {
				r.Header.Add(ForwardedHeader, value)
			}
			if tc.connection != "" {
				r.Header.Set("Connection", tc.connection)
			}
			resp, err := a.srv.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if reached := [2]int{int(a.reached.Load()), int(b.reached.Load())}; resp.StatusCode != tc.status || reached != tc.reached {
				t.Fatalf("answered %d after reaching a and b %v times, want %d after %v", resp.StatusCode, reached, tc.status, tc.reached)
			}
			// The node that refused the write names the leader as it sees it.
			want := map[bool]string{true: net.JoinHostPort("127.0.0.1", strconv.Itoa(b.port())), false: net.JoinHostPort("127.0.0.1", strconv.Itoa(a.port()))}[tc.mark != nil]
			if tc.status == http.StatusServiceUnavailable && resp.Header.Get(response.LeaderHeader) != want {
				t.Fatalf("%s = %q, want %q", response.LeaderHeader, resp.Header.Get(response.LeaderHeader), want)
			}
		})
	}
}

// The leader's answer to a forwarded write loses its CORS headers, whatever the
// case of their names: every Access-Control-* header, and the Origin token of
// Vary. Every other header, and every other Vary token, stays as it was.
func TestDropCORSKeepsTheRest(t *testing.T) {
	res := &http.Response{Header: http.Header{
		"Access-Control-Allow-Origin":      {"https://app.example.com"},
		"Access-Control-Allow-Credentials": {"true"},
		"access-control-expose-headers":    {"ETag"},
		"Vary":                             {"Origin", "Accept-Encoding, origin", " HX-Request "},
		"X-Request-Id":                     {"request-1"},
	}}
	if err := dropCORS(res); err != nil {
		t.Fatal(err)
	}
	want := http.Header{"Vary": {"Accept-Encoding", "HX-Request"}, "X-Request-Id": {"request-1"}}
	if !reflect.DeepEqual(res.Header, want) {
		t.Fatalf("headers %v, want %v", res.Header, want)
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

// proxyChild marks the child process of TestLeaderForwardsWithoutTheProxyOfTheEnvironment.
const proxyChild = "PODIUM_TEST_LEADER_PROXY_CHILD"

// A follower forwards a write to the leader itself, never through the proxy
// that HTTP_PROXY names: through it, the leader would see the proxy as the
// peer of every forwarded write, which it trusts for no forwarding header, so
// every client would share the proxy's address in the login limiter. net/http
// reads the variable once per process, so the forward runs in a child process
// whose environment names a proxy, a listener of the test, which then holds no
// connection. The leader is at an address of TEST-NET-1 (RFC 5737), which a
// proxy would be asked for and nothing answers at: a loopback address skips
// any proxy.
func TestLeaderForwardsWithoutTheProxyOfTheEnvironment(t *testing.T) {
	if os.Getenv(proxyChild) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("a write reached the handler of a follower") })
		w := httptest.NewRecorder()
		Leader(stubLeadership{leader: "192.0.2.1:7000"}, LeaderOptions{ForwardPort: 8080})(next).
			ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/users", nil))
		fmt.Println("forwarded:", w.Code)
		return
	}
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	url := "http://" + proxy.Addr().String()
	child := exec.Command(os.Args[0], "-test.run=^TestLeaderForwardsWithoutTheProxyOfTheEnvironment$")
	child.Env = append(os.Environ(), proxyChild+"=1", "HTTP_PROXY="+url, "http_proxy="+url, "HTTPS_PROXY="+url, "https_proxy="+url, "NO_PROXY=", "no_proxy=")
	out, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "forwarded:") {
		t.Fatalf("the forward in a child process: %v\n%s", err, out)
	}
	// A connection the child opened to the proxy waits in its backlog, which an
	// accept takes at once; a deadline already past would not try one.
	if err := proxy.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if conn, err := proxy.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("the follower forwarded the write through the proxy of the environment")
	}
}
