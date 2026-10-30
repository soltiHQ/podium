package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// crossOriginPost is a POST to /api/v1/users that a browser sends from origin.
func crossOriginPost(origin, site string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
	r.Header.Set("Origin", origin)
	r.Header.Set("Sec-Fetch-Site", site)
	return r.WithContext(httpctx.WithResponder(r.Context(), responder.NewJSON()))
}

func noContent(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

// Only the exact origins of allow_origins may mutate cross-origin; "*" lets
// CORS reads through but trusts no origin with a mutation.
func TestCrossOriginTrustsOnlyListedOrigins(t *testing.T) {
	h := CrossOrigin(CORSConfig{AllowOrigins: []string{"*", "https://app.example.com"}}, zerolog.Nop())(http.HandlerFunc(noContent))
	for _, tc := range []struct {
		origin string
		want   int
	}{
		{"https://app.example.com", http.StatusNoContent},
		{"https://other.example.com", http.StatusForbidden},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, crossOriginPost(tc.origin, "cross-site"))
		if w.Code != tc.want {
			t.Fatalf("POST from %s: HTTP %d, want %d", tc.origin, w.Code, tc.want)
		}
		if tc.want != http.StatusForbidden {
			continue
		}
		var body struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Message != "Cross-origin request blocked." {
			t.Fatalf("refusal body %q (%v), want the message %q", w.Body.String(), err, "Cross-origin request blocked.")
		}
	}
}

// A refusal happens before any route, so it gets no request ID and no access
// log line: CrossOrigin logs it at WARN itself. An allowed request logs nothing.
func TestCrossOriginLogsRefusals(t *testing.T) {
	var logs bytes.Buffer
	h := CrossOrigin(CORSConfig{}, zerolog.New(&logs))(http.HandlerFunc(noContent))

	h.ServeHTTP(httptest.NewRecorder(), crossOriginPost("https://other.example.com", "cross-site"))
	var entry struct {
		Level        string `json:"level"`
		Message      string `json:"message"`
		Method       string `json:"method"`
		Path         string `json:"path"`
		SecFetchSite string `json:"sec_fetch_site"`
		Origin       string `json:"origin"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("refusal log %q: %v", logs.String(), err)
	}
	if entry.Level != "warn" || entry.Message != "cross-origin request blocked" || entry.Method != http.MethodPost ||
		entry.Path != "/api/v1/users" || entry.SecFetchSite != "cross-site" || entry.Origin != "https://other.example.com" {
		t.Fatalf("refusal log %+v, want a WARN line with the method, path, Sec-Fetch-Site and Origin", entry)
	}

	logs.Reset()
	h.ServeHTTP(httptest.NewRecorder(), crossOriginPost("https://podium.example.com", "same-origin"))
	if logs.Len() != 0 {
		t.Fatalf("allowed request logged %q", logs.String())
	}
}
