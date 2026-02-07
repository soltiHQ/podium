package middleware

import (
	"net/http"
	"strings"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// Auth returns middleware that verifies the access token
// and stores the identity in context.
//
// Requests without a token or with an invalid token receive 401.
// Use after RequestID and Negotiate in the chain.
func Auth(verifier token.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractBearer(r)
			if raw == "" {
				response.FromContext(r.Context()).Error(w, r, http.StatusUnauthorized, "missing token")
				return
			}

			id, err := verifier.Verify(r.Context(), raw)
			if err != nil {
				response.FromContext(r.Context()).Error(w, r, http.StatusUnauthorized, "invalid token")
				return
			}

			ctx := transportctx.WithIdentity(r.Context(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequirePermission returns middleware that checks identity
// for a specific permission. Returns 403 if missing.
//
// Must be placed after Auth in the chain.
func RequirePermission(perm kind.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := transportctx.Identity(r.Context())
			if !ok {
				response.FromContext(r.Context()).Error(w, r, http.StatusUnauthorized, "missing identity")
				return
			}

			if !id.HasPermission(perm) {
				response.FromContext(r.Context()).Error(w, r, http.StatusForbidden, "insufficient permissions")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	// "Bearer <token>"
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
