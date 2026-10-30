package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
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
