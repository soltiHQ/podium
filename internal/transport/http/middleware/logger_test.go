package middleware

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// The session_id cookie authenticates a refresh, so it stays out of the logs.
func TestAccessLogOmitsSessionID(t *testing.T) {
	var logs bytes.Buffer
	h := Logger(zerolog.New(&logs))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	r.AddCookie(&http.Cookie{Name: httpcookie.NameSessionID, Value: "secret-session-id"})
	h.ServeHTTP(httptest.NewRecorder(), r)

	out := logs.String()
	if !strings.Contains(out, `"path":"/api/v1/users"`) {
		t.Fatalf("request not logged: %s", out)
	}
	if strings.Contains(out, "secret-session-id") || strings.Contains(out, "session_id") {
		t.Fatalf("access log carries the session ID: %s", out)
	}
}

// The access log is INFO, WARN from 400 and ERROR from 500, except a 503 that
// carries X-Leader: a write that reached a follower, or lost leadership, is a
// routing condition and not a failure of this node, so it is a WARN. Any other
// 5xx stays an ERROR, a 502 that names the leader it could not reach included.
func TestAccessLogLevelFollowsTheStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		leader string // the X-Leader of the answer
		want   string // the level of the line
	}{
		{"ok", http.StatusOK, "", "info"},
		{"client error", http.StatusNotFound, "", "warn"},
		{"server error", http.StatusInternalServerError, "", "error"},
		{"unavailable", http.StatusServiceUnavailable, "", "error"},
		{"unavailable at a follower that knows the leader", http.StatusServiceUnavailable, "10.0.0.2:8080", "warn"},
		{"a leader that cannot be reached", http.StatusBadGateway, "10.0.0.2:8080", "error"},
		{"a server error that names a leader", http.StatusInternalServerError, "10.0.0.2:8080", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := Logger(zerolog.New(&logs))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				response.SetLeader(w, tc.leader)
				w.WriteHeader(tc.status)
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/users", nil))

			var line struct {
				Level  string `json:"level"`
				Status int    `json:"status"`
			}
			if err := json.Unmarshal(logs.Bytes(), &line); err != nil || line.Level != tc.want || line.Status != tc.status {
				t.Fatalf("logged %q, want one %s line with status %d", logs.String(), tc.want, tc.status)
			}
		})
	}
}

// A 429 that carries Retry-After is a throttle: a client sent more than its
// share and was told when to come back, which any client can cause at will, so
// the access log records it at DEBUG, one line per call, not as a WARN. Any
// other 429, such as the login limiter's, stays a WARN, and only a 429 is a
// throttle. A follower proxies the leader's answer with its headers, so it logs
// a forwarded throttle at DEBUG too.
func TestAccessLogOfAThrottleIsDebug(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
		want   string // the level of the line
	}{
		{"a throttle", func(w http.ResponseWriter, r *http.Request) {
			response.TooManyRequests(w, r, httpctx.RenderBlock, time.Second, "slow down")
		}, "debug"},
		{"a 429 without Retry-After", func(w http.ResponseWriter, r *http.Request) {
			response.AuthRateLimit(w, r, httpctx.RenderBlock)
		}, "warn"},
		{"a 503 with Retry-After", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(response.RetryAfterHeader, "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		}, "error"},
		{"a 200 with Retry-After", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(response.RetryAfterHeader, "1")
			w.WriteHeader(http.StatusOK)
		}, "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := RequestID()(Logger(zerolog.New(&logs))(http.HandlerFunc(tc.answer)))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/discovery/sync", nil))

			var line struct {
				Level string `json:"level"`
			}
			if err := json.Unmarshal(logs.Bytes(), &line); err != nil || line.Level != tc.want {
				t.Fatalf("logged %q, want one %s line", logs.String(), tc.want)
			}
		})
	}
}

// lineSink collects what a logger writes and signals each write: the server's
// goroutine logs a request after the client has its answer, so a test waits for
// the line instead of guessing when it comes.
type lineSink struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines chan struct{}
}

func newLineSink() *lineSink { return &lineSink{lines: make(chan struct{}, 16)} }

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.lines <- struct{}{}:
	default:
	}
	return s.buf.Write(p)
}

func (s *lineSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A follower that proxies the leader's 429 logs it as the leader does: the
// headers of the answer are the leader's, so a throttle, which carries
// Retry-After, is a DEBUG there too, and any other 429 is a WARN.
func TestAccessLogOfAForwardedThrottleIsDebug(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request) // the leader's
		want   string                                       // the level of the follower's line
	}{
		{"a throttle", func(w http.ResponseWriter, r *http.Request) {
			response.TooManyRequests(w, r, httpctx.RenderBlock, time.Second, "slow down")
		}, "debug"},
		{"another 429", func(w http.ResponseWriter, r *http.Request) {
			response.AuthRateLimit(w, r, httpctx.RenderBlock)
		}, "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leader := httptest.NewServer(http.HandlerFunc(tc.answer))
			t.Cleanup(leader.Close)
			logs := newLineSink()
			follower := httptest.NewServer(RequestID()(Logger(zerolog.New(logs))(
				Leader(stubLeadership{leader: "127.0.0.1:7000"}, LeaderOptions{ForwardPort: leader.Listener.Addr().(*net.TCPAddr).Port})(
					http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the follower served a write itself") })))))
			t.Cleanup(follower.Close)

			resp, err := follower.Client().Post(follower.URL+"/api/v1/discovery/sync", "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("the follower answered %d, want the leader's 429", resp.StatusCode)
			}
			select {
			case <-logs.lines:
			case <-time.After(5 * time.Second):
				t.Fatal("the follower logged nothing")
			}
			var line struct {
				Level string `json:"level"`
			}
			if err := json.Unmarshal([]byte(logs.String()), &line); err != nil || line.Level != tc.want {
				t.Fatalf("the follower logged %q, want one %s line", logs.String(), tc.want)
			}
		})
	}
}
