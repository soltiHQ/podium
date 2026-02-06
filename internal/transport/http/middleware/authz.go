package middleware

import (
	"net/http"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// RequirePermission checks that identity exists and contains the permission.
func RequirePermission(p kind.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := transportctx.Identity(r.Context())
			if !ok || id == nil {
				_ = response.Unauthorized(r.Context(), w, r, "missing identity")
				return
			}
			if !id.HasPermission(p) {
				_ = response.Forbidden(r.Context(), w, r, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
