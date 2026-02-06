package middleware

import (
	"net/http"
	"strings"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// RequestID ensures a request id exists in context and echoes it back.
// It trusts an incoming header if present; otherwise generates a new id.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := transportctx.DefaultRequestIDHeader

		rid := transportctx.NormalizeRequestID(r.Header.Get(hdr))
		if rid == "" {
			rid = transportctx.NewRequestID()
		}

		w.Header().Set(canonicalHeader(hdr), rid)
		next.ServeHTTP(w, r.WithContext(transportctx.WithRequestID(r.Context(), rid)))
	})
}

func canonicalHeader(h string) string {
	parts := strings.Split(h, "-")

	for i := range parts {
		if len(parts[i]) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(parts[i][:1]) + strings.ToLower(parts[i][1:])
	}
	return strings.Join(parts, "-")
}
