package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     CORSConfig
		wantErr bool
	}{
		{"wildcard without credentials is ok", CORSConfig{AllowOrigins: []string{"*"}}, false},
		{"wildcard with credentials is rejected", CORSConfig{AllowOrigins: []string{"*"}, AllowCredentials: true}, true},
		{"specific origin with credentials is ok", CORSConfig{AllowOrigins: []string{"https://app.example.com"}, AllowCredentials: true}, false},
		{"no origins is ok", CORSConfig{}, false},
		{"origin with a port is ok", CORSConfig{AllowOrigins: []string{"https://app.example.com:8443"}}, false},
		{"origin with a path is rejected", CORSConfig{AllowOrigins: []string{"https://app.example.com/"}}, true},
		{"origin without a scheme is rejected", CORSConfig{AllowOrigins: []string{"app.example.com"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func corsHandler(cfg CORSConfig) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return CORS(cfg)(next)
}

func TestCORSAllowedOriginIsReflected(t *testing.T) {
	h := corsHandler(CORSConfig{AllowOrigins: []string{"https://app.example.com"}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("Allow-Origin = %q, want reflected origin", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}
}

func TestCORSWildcardWithoutCredentials(t *testing.T) {
	h := corsHandler(CORSConfig{AllowOrigins: []string{"*"}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://any.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want *", got)
	}
}

func TestCORSDisallowedOriginPreflightForbidden(t *testing.T) {
	h := corsHandler(CORSConfig{AllowOrigins: []string{"https://app.example.com"}})
	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("preflight from disallowed origin: code = %d, want 403", w.Code)
	}
}

func TestCORSDisallowedOriginSimpleRequestPassesWithoutHeaders(t *testing.T) {
	h := corsHandler(CORSConfig{AllowOrigins: []string{"https://app.example.com"}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (handler runs)", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want empty for disallowed origin", got)
	}
}

func TestCORSPreflightAllowed(t *testing.T) {
	h := corsHandler(CORSConfig{AllowOrigins: []string{"https://app.example.com"}})
	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight code = %d, want 204", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Fatalf("missing Access-Control-Allow-Methods on preflight")
	}
}

func TestCORSNoOriginPassesThrough(t *testing.T) {
	h := corsHandler(CORSConfig{AllowOrigins: []string{"https://app.example.com"}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want empty (no Origin header)", got)
	}
}

// By default a cross-origin client may read a placement's ETag and send it back
// as If-Match on Deploy, next to the headers it could always use.
func TestCORSDefaultsCarryTheDeployPrecondition(t *testing.T) {
	h := corsHandler(CORSConfig{AllowOrigins: []string{"https://app.example.com"}})
	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got, want := w.Header().Get("Access-Control-Allow-Headers"), "Authorization, Content-Type, X-Request-Id, If-Match"; got != want {
		t.Fatalf("Allow-Headers = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("Access-Control-Expose-Headers"), "X-Request-Id, ETag"; got != want {
		t.Fatalf("Expose-Headers = %q, want %q", got, want)
	}
}
