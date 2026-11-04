package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
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
