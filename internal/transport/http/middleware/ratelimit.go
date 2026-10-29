package middleware

import (
	"net/http"
	"time"

	"github.com/felixge/httpsnoop"

	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
)

// RateLimit wraps the handler with a per-IP limit on authentication failures.
// The client IP comes from resolver (a nil resolver trusts no proxy).
//
// Semantics:
//
//   - Before the handler: a blocked IP gets 429 and the handler does not run.
//   - After the handler: a 401 response records a failure for the IP.
//     Other statuses neither count nor reset anything.
func RateLimit(limiter *ratelimit.Limiter, resolver *clientip.Resolver) func(http.Handler) http.Handler {
	if limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := ratelimit.IPKey(resolver.IP(r))
			if limiter.Blocked(key, time.Now()) {
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}

			m := httpsnoop.CaptureMetrics(next, w, r)
			if m.Code == http.StatusUnauthorized {
				limiter.RecordFailure(key, time.Now())
			}
		})
	}
}
