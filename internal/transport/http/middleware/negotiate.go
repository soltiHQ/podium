package middleware

import (
	"net/http"
	"strings"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// Negotiate attaches the correct Responder to the request context.
//
// Policy:
//   - /api/*          → JSON
//   - everything else → HTML
func Negotiate(json *responder.JSONResponder, html *responder.HTMLResponder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var resp responder.Responder
			if strings.HasPrefix(r.URL.Path, "/api/") {
				resp = json
			} else {
				resp = html
			}
			next.ServeHTTP(w, r.WithContext(httpctx.WithResponder(r.Context(), resp)))
		})
	}
}
