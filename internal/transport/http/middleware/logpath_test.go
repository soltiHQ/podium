package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

// A session path keeps nothing of its session ID, whatever the ID holds and
// however many slashes surround it; every other path is logged as it is.
func TestLogPathHidesTheSessionID(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/api/v1/session/secret-session-id/revoke", "/api/v1/session/{id}/revoke"},
		{"/api/v1/session/secret-session-id", "/api/v1/session/{id}"},
		{"/api/v1/session/secret-session-id/", "/api/v1/session/{id}/"},
		{"/api/v1/session/secret/session-id/revoke", "/api/v1/session/{id}/revoke"},
		{"/api/v1/session//secret-session-id/revoke", "/api/v1/session/{id}/revoke"},
		{"/api/v1/session/", "/api/v1/session/"},
		{"/api/v1/session", "/api/v1/session"},
		{"/api/v1/sessions/x", "/api/v1/sessions/x"},
		{"/api/v1/users/me/sessions", "/api/v1/users/me/sessions"},
		{"/", "/"},
	} {
		if got := logPath(tc.path); got != tc.want {
			t.Errorf("logPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// Every middleware line that names the path hides the session ID of a session
// path: the access log, a recovered panic, a cross-origin refusal and both auth
// failures. The request goes to the path the UI builds, so a route that moves
// fails here.
func TestLoggedPathsHideTheSessionID(t *testing.T) {
	if sessionPath != routepath.ApiSession {
		t.Fatalf("sessionPath = %q, want routepath.ApiSession, %q", sessionPath, routepath.ApiSession)
	}
	const secret = "secret-session-id"
	target := routepath.ApiUserRevokeSession(secret)
	want := routepath.ApiSession + "{id}/revoke"
	for _, tc := range []struct {
		name string
		log  func(zerolog.Logger, *http.Request)
	}{
		{"access log", func(l zerolog.Logger, r *http.Request) {
			Logger(l)(http.HandlerFunc(noContent)).ServeHTTP(httptest.NewRecorder(), r)
		}},
		{"recovered panic", func(l zerolog.Logger, r *http.Request) {
			Recovery(l)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })).ServeHTTP(httptest.NewRecorder(), r)
		}},
		{"cross-origin refusal", func(l zerolog.Logger, r *http.Request) {
			r.Header.Set("Origin", "https://other.example.com")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			CrossOrigin(CORSConfig{}, l)(http.HandlerFunc(noContent)).ServeHTTP(httptest.NewRecorder(), r)
		}},
		{"auth check failure", func(l zerolog.Logger, r *http.Request) {
			logCheckFailure(l, r, errors.New("store unreachable"))
		}},
		{"auth refresh off the leader", func(l zerolog.Logger, r *http.Request) {
			checkFailed(httptest.NewRecorder(), r, httpctx.ModeFromRequest(r), l, cluster.ErrNotLeader)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			r := httptest.NewRequest(http.MethodPost, target, nil)
			r = r.WithContext(httpctx.WithResponder(r.Context(), responder.NewJSON()))
			tc.log(zerolog.New(&logs), r)

			var entry struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("want one log line, got %q: %v", logs.String(), err)
			}
			if entry.Path != want {
				t.Fatalf("path %q, want %q", entry.Path, want)
			}
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("the log carries the session ID: %s", logs.String())
			}
		})
	}
}
