package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORSConfig controls CORS behavior.
type CORSConfig struct {
	// AllowOrigins is the list of allowed origins.
	// Use ["*"] to allow all (not recommended with credentials).
	AllowOrigins []string

	// AllowMethods is the list of allowed HTTP methods.
	// Defaults to GET, POST, PUT, DELETE, PATCH, OPTIONS.
	AllowMethods []string

	// AllowHeaders is the list of allowed request headers.
	// Defaults to Authorization, Content-Type, X-Request-Id.
	AllowHeaders []string

	// ExposeHeaders is the list of headers the browser can access.
	// Defaults to X-Request-Id.
	ExposeHeaders []string

	// AllowCredentials indicates whether cookies/auth headers are allowed.
	AllowCredentials bool

	// MaxAge is how long the browser caches preflight results.
	// Defaults to 12 hours.
	MaxAge time.Duration
}

func (c CORSConfig) withDefaults() CORSConfig {
	if len(c.AllowMethods) == 0 {
		c.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}
	}
	if len(c.AllowHeaders) == 0 {
		c.AllowHeaders = []string{"Authorization", "Content-Type", "X-Request-Id"}
	}
	if len(c.ExposeHeaders) == 0 {
		c.ExposeHeaders = []string{"X-Request-Id"}
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 12 * time.Hour
	}
	return c
}

// CORS returns middleware that handles Cross-Origin Resource Sharing.
//
// Preflight (OPTIONS) requests are answered immediately.
// Actual requests get CORS headers attached.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	cfg = cfg.withDefaults()

	allowMethods := strings.Join(cfg.AllowMethods, ", ")
	allowHeaders := strings.Join(cfg.AllowHeaders, ", ")
	exposeHeaders := strings.Join(cfg.ExposeHeaders, ", ")
	maxAge := strconv.Itoa(int(cfg.MaxAge.Seconds()))

	origins := make(map[string]struct{}, len(cfg.AllowOrigins))
	allowAll := false
	for _, o := range cfg.AllowOrigins {
		if o == "*" {
			allowAll = true
			break
		}
		origins[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Not a CORS request — pass through.
				next.ServeHTTP(w, r)
				return
			}

			if !allowAll {
				if _, ok := origins[origin]; !ok {
					// Origin not allowed — pass through without CORS headers.
					next.ServeHTTP(w, r)
					return
				}
			}

			// Reflect the actual origin (required when credentials: true).
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if cfg.AllowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Access-Control-Expose-Headers", exposeHeaders)

			// Vary so caches distinguish by origin.
			w.Header().Add("Vary", "Origin")

			// Preflight.
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", allowMethods)
				w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
				w.Header().Set("Access-Control-Max-Age", maxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
