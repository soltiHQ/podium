package middleware

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// RequestID attaches a unique request ID to the request context.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := transportctx.NormalizeRequestID(r.Header.Get(transportctx.DefaultRequestIDHeader))
			if rid == "" {
				rid = transportctx.NewRequestID()
			}
			w.Header().Set(transportctx.DefaultRequestIDHeader, rid)
			next.ServeHTTP(w, r.WithContext(transportctx.WithRequestID(r.Context(), rid)))
		})
	}
}
