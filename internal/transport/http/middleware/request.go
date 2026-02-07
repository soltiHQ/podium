package middleware

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := transportctx.NormalizeRequestID(r.Header.Get(transportctx.DefaultRequestIDHeader))
			if rid == "" {
				rid = transportctx.NewRequestID()
			}

			ctx := transportctx.WithRequestID(r.Context(), rid)

			w.Header().Set(transportctx.DefaultRequestIDHeader, rid)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
