package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// followerStore is the store of a raft follower: its reads work and every
// transaction fails, as raft.Store.WithTx does, with err, or with ErrNotLeader
// when it is nil.
type followerStore struct {
	storage.Storage
	err error
}

func (s followerStore) WithTx(context.Context, func(storage.Storage) error) error {
	if s.err != nil {
		return s.err
	}
	return fmt.Errorf("raft: WithTx: %w", cluster.ErrNotLeader)
}

// followerRefresh returns what a browser with an expired access token needs to
// refresh at a follower whose transactions fail with err: the verifier and the
// session service of the node, over a store that holds the browser's session,
// with rotation on, and the cookies of the browser.
func followerRefresh(t *testing.T, err error) (token.Verifier, *session.Service, []*http.Cookie) {
	t.Helper()
	st, _, cookies := cookieRefreshFixture(t)
	clk := token.RealClock()
	secret := []byte("test-only-local-refresh-cookie-signing-key")
	svc := session.New(followerStore{Storage: st, err: err}, jwt.NewHSIssuer(secret, clk), clk, session.Config{AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, Issuer: "cookie-test", Audience: "cookie-client", RotateRefresh: true}, rbac.NewResolver(st), nil)
	return jwt.NewHSVerifier("cookie-test", "cookie-client", secret, clk), svc, cookies
}

// With auth.rotate_refresh a refresh rotates the refresh token, which only the
// leader can write. A browser whose access token has expired and whose request
// reaches a follower, or reaches a node that loses leadership, or shuts down,
// as the rotation commits, gets 503, with the leader's address in X-Leader when
// the node knows it, and no cookie; Auth logs one WARN, no ERROR, and never a
// credential, worded for what happened: a follower that cannot rotate the
// token, or a rotation whose outcome is unknown. Auth and OptionalAuth alike.
func TestRefreshOnAFollowerAsksForTheLeader(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error  // what the transaction fails with; nil for a follower
		says string // what the WARN says happened
		not  string // what it must not say
	}{
		{"not the leader", nil, "a follower cannot rotate a refresh token", "outcome unknown"},
		{"leadership lost", fmt.Errorf("raft: apply: %w", cluster.ErrLeadershipLost), "outcome unknown", "a follower"},
	} {
		verifier, svc, cookies := followerRefresh(t, failure.err)
		for _, mw := range []struct {
			name string
			wrap func(token.Verifier, *session.Service, func(*http.Request) bool, zerolog.Logger) func(http.Handler) http.Handler
		}{{"Auth", Auth}, {"OptionalAuth", OptionalAuth}} {
			for _, tc := range []struct {
				name       string
				leadership stubLeadership
				want       string // X-Leader
			}{
				{"leader known", stubLeadership{leader: "10.0.0.7:7000"}, "10.0.0.7:8080"},
				{"no leader known", stubLeadership{}, ""},
			} {
				t.Run(failure.name+", "+mw.name+", "+tc.name, func(t *testing.T) {
					var logs bytes.Buffer
					next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("request reached the handler") })
					handler := Leader(tc.leadership, LeaderOptions{ForwardPort: 8080})(mw.wrap(verifier, svc, notSecure, zerolog.New(&logs))(next))
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, cookieRefreshRequest(t.Context(), "/ui/specs?q=1", cookies))

					if w.Code != http.StatusServiceUnavailable {
						t.Fatalf("HTTP %d, want 503", w.Code)
					}
					if got := w.Header().Get("X-Leader"); got != tc.want {
						t.Fatalf("X-Leader = %q, want %q", got, tc.want)
					}
					if set := w.Result().Cookies(); len(set) != 0 {
						t.Fatalf("the answer sets cookies: %v", set)
					}
					var entry struct {
						Level   string `json:"level"`
						Method  string `json:"method"`
						Path    string `json:"path"`
						Message string `json:"message"`
					}
					if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "warn" || entry.Method != http.MethodGet ||
						entry.Path != "/ui/specs" || !strings.Contains(entry.Message, "auth.rotate_refresh") {
						t.Fatalf("log %q, want one WARN line with the method, the path and auth.rotate_refresh", logs.String())
					}
					if !strings.Contains(entry.Message, failure.says) || strings.Contains(entry.Message, failure.not) {
						t.Fatalf("the WARN says %q, want %q and not %q", entry.Message, failure.says, failure.not)
					}
					for _, secret := range []string{"test-only-original-refresh", "cookie-session"} {
						if strings.Contains(logs.String(), secret) {
							t.Fatalf("log holds the credential %q: %s", secret, logs.String())
						}
					}
				})
			}
		}
	}
}

// On an API route the access log wraps Auth. Through the chain that runs there,
// the refresh on a follower logs the WARN of Auth and the access log's own line,
// whose 503 carries X-Leader and so is a WARN too: the routing condition leaves
// no ERROR line, where every other 503 of the access log is one.
func TestFollowerRefreshThroughTheAccessLogLogsNoError(t *testing.T) {
	verifier, svc, cookies := followerRefresh(t, nil)
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("request reached the handler") })
	// Leader, RequestID, Logger and Auth in the order of an API route.
	handler := Leader(stubLeadership{leader: "10.0.0.7:7000"}, LeaderOptions{ForwardPort: 8080})(
		RequestID()(Logger(logger)(Auth(verifier, svc, notSecure, logger)(next))))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, cookieRefreshRequest(t.Context(), "/api/v1/specs", cookies))

	if w.Code != http.StatusServiceUnavailable || w.Header().Get("X-Leader") != "10.0.0.7:8080" {
		t.Fatalf("HTTP %d with X-Leader %q, want 503 with the leader's address", w.Code, w.Header().Get("X-Leader"))
	}
	type line struct {
		Level   string `json:"level"`
		Message string `json:"message"`
		Status  int    `json:"status"`
		Path    string `json:"path"`
	}
	var lines []line
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		lines = append(lines, l)
	}
	if len(lines) != 2 ||
		lines[0].Level != "warn" || !strings.Contains(lines[0].Message, "auth.rotate_refresh") ||
		lines[1].Level != "warn" || lines[1].Message != "http request" || lines[1].Status != http.StatusServiceUnavailable || lines[1].Path != "/api/v1/specs" {
		t.Fatalf("log %q, want the WARN of Auth and the WARN of the access log for the 503, and no ERROR", logs.String())
	}
}
