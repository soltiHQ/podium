package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
)

// statusByPath answers /unauthorized with 401, /bad with 400 and anything else with 200.
var statusByPath = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/unauthorized":
		w.WriteHeader(http.StatusUnauthorized)
	case "/bad":
		w.WriteHeader(http.StatusBadRequest)
	default:
		w.WriteHeader(http.StatusOK)
	}
})

func newIPLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{MaxAttempts: 5, Window: time.Minute, BlockWindow: time.Minute})
}

// serve sends GET path from the TCP peer remoteAddr; forwardedFor, when set, is the X-Forwarded-For header.
func serve(h http.Handler, path, remoteAddr, forwardedFor string) int {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		r.Header.Set("X-Forwarded-For", forwardedFor)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestNonAuthErrorsDoNotLockOutIP(t *testing.T) {
	h := RateLimit(newIPLimiter(), nil)(statusByPath)
	for i := range 10 {
		if code := serve(h, "/bad", "203.0.113.9:4711", ""); code != http.StatusBadRequest {
			t.Fatalf("request %d: HTTP %d, want 400", i+1, code)
		}
	}
	if code := serve(h, "/", "203.0.113.9:4711", ""); code != http.StatusOK {
		t.Fatalf("GET after ten 400 responses: HTTP %d, want 200", code)
	}
}

func TestAuthFailuresLockOutIP(t *testing.T) {
	h := RateLimit(newIPLimiter(), nil)(statusByPath)
	for i := range 5 {
		if code := serve(h, "/unauthorized", "203.0.113.9:4711", ""); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: HTTP %d, want 401", i+1, code)
		}
		// A success in between does not reset the count.
		if i < 4 {
			if code := serve(h, "/", "203.0.113.9:4711", ""); code != http.StatusOK {
				t.Fatalf("success after failure %d: HTTP %d, want 200", i+1, code)
			}
		}
	}
	if code := serve(h, "/", "203.0.113.9:4711", ""); code != http.StatusTooManyRequests {
		t.Fatalf("after five 401 responses: HTTP %d, want 429", code)
	}
	if code := serve(h, "/", "198.51.100.7:4711", ""); code != http.StatusOK {
		t.Fatalf("another client IP: HTTP %d, want 200", code)
	}
}

func TestRateLimitKeysByResolvedClientIP(t *testing.T) {
	resolver, err := clientip.New([]string{"10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	h := RateLimit(newIPLimiter(), resolver)(statusByPath)
	const proxy = "10.0.0.1:443"
	for range 5 {
		serve(h, "/unauthorized", proxy, "203.0.113.9")
	}
	if code := serve(h, "/", proxy, "203.0.113.9"); code != http.StatusTooManyRequests {
		t.Fatalf("blocked client behind the proxy: HTTP %d, want 429", code)
	}
	if code := serve(h, "/", proxy, "198.51.100.7"); code != http.StatusOK {
		t.Fatalf("another client behind the same proxy: HTTP %d, want 200", code)
	}
}
