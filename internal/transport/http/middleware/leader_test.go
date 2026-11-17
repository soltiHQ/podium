package middleware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
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
			refused, forwarded := 0, 0
			// The stub's view of the leader never moves: no wait for a handover.
			opts := LeaderOptions{ForwardPort: tt.port, Refused: func() { refused++ }, Forwarded: func() { forwarded++ }, HandoverWait: -1}
			Leader(tt.leadership, opts)(next).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if refused != 1 {
				t.Fatalf("Refused called %d times for the refusal, want once", refused)
			}
			// Only the leader that cannot be reached was sent the write.
			if want := map[bool]int{true: 1, false: 0}[tt.status == http.StatusBadGateway]; forwarded != want {
				t.Fatalf("Forwarded called %d times, want %d", forwarded, want)
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

// Forwarded hears each write a follower sends to the leader, once, whatever the
// leader answers; never a read, a write on the leader, or a write the follower
// refuses without sending it, as one another node forwarded already.
func TestLeaderCountsTheWritesItForwards(t *testing.T) {
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) }))
	t.Cleanup(leader.Close)
	port := leader.Listener.Addr().(*net.TCPAddr).Port
	served := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		name      string
		leading   bool
		method    string
		marked    bool
		status    int
		forwarded int
	}{
		{"a write on a follower", false, http.MethodPost, false, http.StatusConflict, 1},
		{"a read on a follower", false, http.MethodGet, false, http.StatusNoContent, 0},
		{"a write on the leader", true, http.MethodPost, false, http.StatusNoContent, 0},
		{"a write forwarded already", false, http.MethodPost, true, http.StatusServiceUnavailable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forwarded := 0
			opts := LeaderOptions{ForwardPort: port, Forwarded: func() { forwarded++ }}
			h := Leader(stubLeadership{leading: tc.leading, leader: "127.0.0.1:7000"}, opts)(served)
			r := httptest.NewRequest(tc.method, "/api/v1/users", nil)
			if tc.marked {
				r.Header.Set(ForwardedHeader, "1")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || forwarded != tc.forwarded {
				t.Fatalf("answered %d with %d forwards heard, want %d with %d", w.Code, forwarded, tc.status, tc.forwarded)
			}
		})
	}
}

// logLines decodes the JSON lines of buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// A write that a follower answers itself, because it cannot send it to a
// leader, goes to Log, when set, with why: WARN for a 503 that names the
// leader, a routing answer, and ERROR for any other, as the access log ranks
// them, and the reason in the request's error slot too, for the access log of
// a listener that wraps the middleware. A leader that cannot be reached is
// logged with what the forward met, so that a refused connection and a leader
// certificate that does not verify read apart.
func TestLeaderLogsTheWritesItRefuses(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.Addr().(*net.TCPAddr).Port
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	// A leader whose certificate httptest made, which no follower trusts.
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
	untrusted.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(untrusted.Close)
	untrustedPort := untrusted.Listener.Addr().(*net.TCPAddr).Port

	for _, tc := range []struct {
		name       string
		leadership stubLeadership
		port       int
		tls        bool
		marked     bool
		status     int
		level      string
		leader     string // X-Leader, and the leader of the log line
		reason     string // in the log line's error and the error slot
	}{
		{"no leader known", stubLeadership{}, 8080, false, false, http.StatusServiceUnavailable, "error", "", "no leader is known"},
		{"no port to forward to", stubLeadership{leader: "10.0.0.1:7000"}, 0, false, false, http.StatusServiceUnavailable, "error", "", "no port to forward to"},
		{"a write forwarded already", stubLeadership{leader: "10.0.0.1:7000"}, 8080, false, true, http.StatusServiceUnavailable, "warn", "10.0.0.1:8080", "forwarded here already"},
		{"a leader that refuses the connection", stubLeadership{leader: "127.0.0.1:7000"}, unreachable, false, false, http.StatusBadGateway, "error", net.JoinHostPort("127.0.0.1", strconv.Itoa(unreachable)), "connection refused"},
		{"a leader certificate that does not verify", stubLeadership{leader: "127.0.0.1:7000"}, untrustedPort, true, false, http.StatusBadGateway, "error", net.JoinHostPort("127.0.0.1", strconv.Itoa(untrustedPort)), "certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			opts := LeaderOptions{ForwardPort: tc.port, TargetTLS: tc.tls, HandoverWait: -1, Log: zerolog.New(&buf)}
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("a write reached the handler of a follower") })
			r := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader("{}"))
			r = r.WithContext(transportctx.WithErrorSlot(r.Context()))
			if tc.marked {
				r.Header.Set(ForwardedHeader, "1")
			}
			w := httptest.NewRecorder()
			Leader(tc.leadership, opts)(next).ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get(response.LeaderHeader) != tc.leader {
				t.Fatalf("answered %d with %s %q, want %d with %q", w.Code, response.LeaderHeader, w.Header().Get(response.LeaderHeader), tc.status, tc.leader)
			}
			if slot := transportctx.TryError(r.Context()); !strings.Contains(slot, tc.reason) {
				t.Fatalf("error slot %q, want one with %q", slot, tc.reason)
			}
			lines := logLines(t, &buf)
			if len(lines) != 1 {
				t.Fatalf("logged %d lines, want 1: %s", len(lines), buf.String())
			}
			line := lines[0]
			reason, _ := line["error"].(string)
			if line["level"] != tc.level || line["status"] != float64(tc.status) || line["method"] != http.MethodPost || line["path"] != "/api/v1/users" || !strings.Contains(reason, tc.reason) {
				t.Fatalf("log line %v, want level %s, status %d, the method and path, an error with %q", line, tc.level, tc.status, tc.reason)
			}
			if leader, _ := line["leader"].(string); leader != tc.leader {
				t.Fatalf("log line names the leader %q, want %q", leader, tc.leader)
			}
		})
	}

	t.Run("without Log", func(t *testing.T) {
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("a write reached the handler of a follower") })
		w := httptest.NewRecorder()
		// The stub never knows a leader: no wait for one.
		Leader(stubLeadership{}, LeaderOptions{ForwardPort: 8080, HandoverWait: -1})(next).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("answered %d, want 503", w.Code)
		}
	})
}

// movingLeadership is a follower's view of the leader that the test moves.
type movingLeadership struct {
	mu      sync.Mutex
	leading bool
	leader  string
}

func (l *movingLeadership) AmLeader() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leading
}

func (l *movingLeadership) CurrentLeader() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leader
}

func (*movingLeadership) WhenLeader(ctx context.Context, _ func(context.Context) error) error {
	<-ctx.Done()
	return nil
}

func (l *movingLeadership) move(leading bool, leader string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leading, l.leader = leading, leader
}

// handoverHosts returns a port that two loopback hosts, 127.0.0.1 for the
// leader that left and 127.0.0.2 for the next one, could both bind, with a
// listener of the next leader's host on it. It skips the test where 127.0.0.2
// is not a loopback address of this host.
func handoverHosts(t *testing.T) (port int, next net.Listener) {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.2:0")
		if err != nil {
			t.Skipf("127.0.0.2 is no loopback address here: %v", err)
		}
		port = l.Addr().(*net.TCPAddr).Port
		probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			_ = l.Close()
			continue
		}
		_ = probe.Close()
		return port, l
	}
	t.Fatal("no port free on both 127.0.0.1 and 127.0.0.2 in 20 attempts")
	return 0, nil
}

// A forward that fails to connect sent the leader nothing of the write, as at
// a handover, when the leader that left refuses the connection: the follower
// waits for its view of the leader to move, and sends the write, body and all,
// to the next leader, or serves it itself once it leads, counting one forward
// and no refusal, and logging the handover. A view that does not move within
// HandoverWait leaves the 502, which names the leader it tried. A forward that
// reached the leader is never sent again, whatever the leader then did: here
// the leader read the write and dropped the connection, and only the leader
// may know whether it ran.
func TestLeaderSendsAWriteAgainAfterAHandover(t *testing.T) {
	const payload = `{"subject":"dave"}`
	for _, tc := range []struct {
		name       string
		old        string        // what the leader that left does with a connection: "refuse" or "drop" it once read
		moveAfter  time.Duration // the view moves this long after the write arrives, or once the old leader read it ("drop")
		leading    bool          // the view moves to this node, instead of the next leader
		wait       time.Duration
		status     int
		nextGot    int // the writes the next leader got
		served     int // the writes this node served itself
		refused    int
		logMessage string // the message of the one log line, "" for none
	}{
		{"to the next leader", "refuse", 200 * time.Millisecond, false, 5 * time.Second, http.StatusCreated, 1, 0, 0, "write sent to the next leader after a handover"},
		{"to this node", "refuse", 200 * time.Millisecond, true, 5 * time.Second, http.StatusNoContent, 0, 1, 0, "write served here after a handover"},
		{"a view that stays", "refuse", 0, false, 300 * time.Millisecond, http.StatusBadGateway, 0, 0, 1, "write not forwarded to the leader"},
		{"a leader that got the write", "drop", 0, false, 5 * time.Second, http.StatusBadGateway, 0, 0, 1, "write not forwarded to the leader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, nextLis := handoverHosts(t)
			var nextGot atomic.Int32
			next := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != payload {
					t.Errorf("the next leader got %q, want %q", body, payload)
				}
				nextGot.Add(1)
				w.WriteHeader(http.StatusCreated)
			})}
			go func() { _ = next.Serve(nextLis) }()
			t.Cleanup(func() { _ = next.Close() })

			view := &movingLeadership{leader: "127.0.0.1:7000"}
			moveNow := func() { view.move(tc.leading, map[bool]string{true: "", false: "127.0.0.2:7000"}[tc.leading]) }
			if tc.old == "drop" {
				// The leader that left reads the write, the view moves on, and the connection drops unanswered.
				old, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = old.Close() })
				go func() {
					conn, err := old.Accept()
					if err != nil {
						return
					}
					req, err := http.ReadRequest(bufio.NewReader(conn))
					if err == nil {
						_, _ = io.ReadAll(req.Body)
					}
					moveNow()
					_ = conn.Close()
				}()
			}

			var served atomic.Int32
			here := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != payload {
					t.Errorf("this node served %q, want %q", body, payload)
				}
				served.Add(1)
				w.WriteHeader(http.StatusNoContent)
			})
			var buf bytes.Buffer
			refused, forwarded := 0, 0
			opts := LeaderOptions{
				ForwardPort: port, HandoverWait: tc.wait, Log: zerolog.New(&buf),
				Refused: func() { refused++ }, Forwarded: func() { forwarded++ },
			}
			if tc.old == "refuse" && tc.moveAfter > 0 {
				time.AfterFunc(tc.moveAfter, moveNow)
			}
			w := httptest.NewRecorder()
			start := time.Now()
			Leader(view, opts)(here).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(payload)))
			took := time.Since(start)

			if w.Code != tc.status || int(nextGot.Load()) != tc.nextGot || int(served.Load()) != tc.served {
				t.Fatalf("answered %d, the next leader got %d writes, this node served %d; want %d, %d, %d",
					w.Code, nextGot.Load(), served.Load(), tc.status, tc.nextGot, tc.served)
			}
			if forwarded != 1 || refused != tc.refused {
				t.Fatalf("heard %d forwards and %d refusals, want 1 and %d", forwarded, refused, tc.refused)
			}
			if tc.status == http.StatusBadGateway && w.Header().Get(response.LeaderHeader) != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) {
				t.Fatalf("%s = %q, want the leader it tried", response.LeaderHeader, w.Header().Get(response.LeaderHeader))
			}
			if tc.name == "a view that stays" && took < tc.wait {
				t.Fatalf("answered after %s, before the wait of %s was over", took, tc.wait)
			}
			lines := logLines(t, &buf)
			if len(lines) != 1 || lines[0]["message"] != tc.logMessage {
				t.Fatalf("logged %v, want one line %q", lines, tc.logMessage)
			}
		})
	}
}

// A follower that knows no leader, as between a leader's release and the
// takeover, holds a write it can forward for up to HandoverWait, as it does
// after a forward that failed to connect: it forwards the write once a leader
// is known, serves it once it leads, and answers 503 when no leader comes.
// A write another node forwarded already is refused at once.
func TestLeaderWaitsForALeaderWhenItKnowsNone(t *testing.T) {
	const payload = `{"subject":"dave"}`
	leaderSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != payload {
			t.Errorf("the leader got %q, want %q", body, payload)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(leaderSrv.Close)
	port := leaderSrv.Listener.Addr().(*net.TCPAddr).Port
	for _, tc := range []struct {
		name    string
		move    func(*movingLeadership) // 200 ms into the write, nil for nothing
		marked  bool
		wait    time.Duration
		status  int
		refused int
		minTook time.Duration
	}{
		{"a leader becomes known", func(l *movingLeadership) { l.move(false, "127.0.0.1:7000") }, false, 5 * time.Second, http.StatusCreated, 0, 200 * time.Millisecond},
		{"this node comes to lead", func(l *movingLeadership) { l.move(true, "") }, false, 5 * time.Second, http.StatusNoContent, 0, 200 * time.Millisecond},
		{"no leader comes", nil, false, 300 * time.Millisecond, http.StatusServiceUnavailable, 1, 300 * time.Millisecond},
		{"a write forwarded already", func(l *movingLeadership) { l.move(false, "127.0.0.1:7000") }, true, 5 * time.Second, http.StatusServiceUnavailable, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := &movingLeadership{}
			if tc.move != nil {
				time.AfterFunc(200*time.Millisecond, func() { tc.move(view) })
			}
			here := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if body, _ := io.ReadAll(r.Body); string(body) != payload {
					t.Errorf("this node served %q, want %q", body, payload)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			refused := 0
			opts := LeaderOptions{ForwardPort: port, HandoverWait: tc.wait, Refused: func() { refused++ }}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(payload))
			if tc.marked {
				r.Header.Set(ForwardedHeader, "1")
			}
			w := httptest.NewRecorder()
			start := time.Now()
			Leader(view, opts)(here).ServeHTTP(w, r)
			took := time.Since(start)
			if w.Code != tc.status || refused != tc.refused {
				t.Fatalf("answered %d with %d refusals, want %d with %d", w.Code, refused, tc.status, tc.refused)
			}
			if took < tc.minTook || (tc.marked && took > 100*time.Millisecond) {
				t.Fatalf("answered after %s, want %s at least, and at once for a marked write", took, tc.minTook)
			}
		})
	}
}

// A forward counts as unsent, which may go to the next leader, only when its
// dial failed, no connection was ever handed to it, not a byte of its body was
// read, and its client still waits. A connection handed out, even one that the
// transport then dropped to retry on a new dial, may have carried the write.
func TestUnsentNeedsNoConnection(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
	readErr := &net.OpError{Op: "read", Net: "tcp", Err: fmt.Errorf("connection reset by peer")}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		read      int64
		connected bool
		err       error
		want      bool
	}{
		{"a dial that failed", context.Background(), 0, false, dialErr, true},
		{"a dial that failed after a connection was handed out", context.Background(), 0, true, dialErr, false},
		{"a body read in part", context.Background(), 3, false, dialErr, false},
		{"a read that failed", context.Background(), 0, false, readErr, false},
		{"a client that gave up", cancelled, 0, false, dialErr, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(tc.ctx, http.MethodPost, "/", nil)
			body := &unsentBody{ReadCloser: http.NoBody}
			body.read.Store(tc.read)
			if got := unsent(r, body, tc.connected, tc.err); got != tc.want {
				t.Fatalf("unsent() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A leader that takes the connection and never answers the TLS handshake, as a
// node that hangs does, fails the forward within about
// cluster.ForwardDialTimeout as a 502 that says so, not after the client's own
// deadline.
func TestLeaderGivesUpOnALeaderThatStallsTheHandshake(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/users", strings.NewReader("{}"))
	r = r.WithContext(transportctx.WithErrorSlot(r.Context()))
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("a write reached the handler of a follower") })
	opts := LeaderOptions{ForwardPort: silent.Addr().(*net.TCPAddr).Port, TargetTLS: true, HandoverWait: -1}
	w := httptest.NewRecorder()
	start := time.Now()
	Leader(stubLeadership{leader: "127.0.0.1:7000"}, opts)(next).ServeHTTP(w, r)
	took := time.Since(start)
	if w.Code != http.StatusBadGateway || !strings.Contains(transportctx.TryError(r.Context()), "TLS handshake timeout") {
		t.Fatalf("answered %d after %s with %q, want 502 for a TLS handshake timeout", w.Code, took, transportctx.TryError(r.Context()))
	}
	if took > 3*cluster.ForwardDialTimeout {
		t.Fatalf("answered after %s, want about %s", took, cluster.ForwardDialTimeout)
	}
}

// A write that the transport sent on a reused connection is never sent to the
// next leader, even when the transport itself retried it on a new dial that
// failed: here a replayable write, a body-less POST with Idempotency-Key, which
// the leader that left read and then dropped the connection of, as it closed
// its listener, so the transport's own retry met a refused connection. The
// follower answers 502, and the next leader never gets the write.
func TestLeaderNeverResendsAWriteThatAReusedConnectionCarried(t *testing.T) {
	port, nextLis := handoverHosts(t)
	var nextGot atomic.Int32
	next := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextGot.Add(1)
		w.WriteHeader(http.StatusCreated)
	})}
	go func() { _ = next.Serve(nextLis) }()
	t.Cleanup(func() { _ = next.Close() })

	view := &movingLeadership{leader: "127.0.0.1:7000"}
	old, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := old.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		// The first write: answered, on a connection kept alive.
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = io.WriteString(conn, "HTTP/1.1 204 No Content\r\n\r\n")
		// The leader leaves: no new connection, and the second write, read on the
		// kept connection, is dropped unanswered once the view has moved.
		_ = old.Close()
		if _, err := http.ReadRequest(reader); err != nil {
			return
		}
		view.move(false, "127.0.0.2:7000")
	}()

	refused := 0
	h := Leader(view, LeaderOptions{ForwardPort: port, HandoverWait: 5 * time.Second, Refused: func() { refused++ }})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("a write reached the handler of a follower") }))
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader("{}")))
	if first.Code != http.StatusNoContent {
		t.Fatalf("the first write answered %d, want the leader's 204", first.Code)
	}
	second := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/restart", nil)
	second.Header.Set("Idempotency-Key", "restart-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, second)
	<-served
	if w.Code != http.StatusBadGateway || nextGot.Load() != 0 || refused != 1 {
		t.Fatalf("the second write answered %d, the next leader got %d writes, %d refusals; want 502, 0, 1", w.Code, nextGot.Load(), refused)
	}
}
