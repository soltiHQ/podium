package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// Only the exact origins of allow_origins may mutate cross-origin; "*" lets
// CORS reads through but trusts no origin with a mutation.
func TestCrossOriginTrustsOnlyListedOrigins(t *testing.T) {
	h := CrossOrigin(CORSConfig{AllowOrigins: []string{"*", "https://app.example.com"}})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	)
	for _, tc := range []struct {
		origin string
		want   int
	}{
		{"https://app.example.com", http.StatusNoContent},
		{"https://other.example.com", http.StatusForbidden},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r = r.WithContext(httpctx.WithResponder(r.Context(), responder.NewJSON()))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
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
