package middleware

import (
	"net/http"
	"sync"

	"github.com/soltiHQ/control-plane/internal/transport/clientip"
)

// StreamLimiter caps the number of concurrent long-lived streams per IP.
// Unlike the failure-based [ratelimit.Limiter], it counts in-flight connections,
// not the error rate - appropriate for SSE/streaming endpoints where a single client legitimately holds the connection open.
type StreamLimiter struct {
	max      int
	resolver *clientip.Resolver

	mu     sync.Mutex
	counts map[string]int
}

// NewStreamLimiter builds a limiter with the given per-IP cap. max <= 0 disables limiting (always Acquire returns true).
// [StreamLimit] takes the client IP from resolver; a nil resolver trusts no proxy.
func NewStreamLimiter(max int, resolver *clientip.Resolver) *StreamLimiter {
	return &StreamLimiter{
		max:      max,
		resolver: resolver,
		counts:   make(map[string]int),
	}
}

// Acquire reserves a slot for ip. Returns false if the cap is reached.
func (l *StreamLimiter) Acquire(ip string) bool {
	if l == nil || l.max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[ip] >= l.max {
		return false
	}
	l.counts[ip]++
	return true
}

// Release returns a slot previously taken by Acquire.
func (l *StreamLimiter) Release(ip string) {
	if l == nil || l.max <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[ip] > 0 {
		l.counts[ip]--
		if l.counts[ip] == 0 {
			delete(l.counts, ip)
		}
	}
}

// StreamLimit wraps a streaming handler with a per-IP concurrency cap.
// Use on long-lived endpoints (SSE log tail, watches).
func StreamLimit(limiter *StreamLimiter) func(http.Handler) http.Handler {
	if limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := limiter.resolver.IP(r)
			if !limiter.Acquire(ip) {
				http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
				return
			}
			defer limiter.Release(ip)
			next.ServeHTTP(w, r)
		})
	}
}
