package middleware

import (
	"net/http"
	"strings"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// Auth returns middleware that verifies the access token
// and stores the identity in context.
//
// Requests without a token or with an invalid token receive 401.
// Use after RequestID and Negotiate in the chain.
func Auth(verifier token.Verifier, sessionSvc *session.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractBearer(r)
			if raw == "" {
				response.FromContext(r.Context()).Error(w, r, http.StatusUnauthorized, "missing token")
				return
			}

			id, err := verifier.Verify(r.Context(), raw)
			if err == nil {
				ctx := transportctx.WithIdentity(r.Context(), id)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Access token invalid/expired — try refresh (cookie-based only).
			if !isCookieBased(r) {
				response.FromContext(r.Context()).Error(w, r, http.StatusUnauthorized, "invalid token")
				return
			}

			id, err = tryRefresh(w, r, sessionSvc)
			if err != nil {
				response.FromContext(r.Context()).Error(w, r, http.StatusUnauthorized, "session expired")
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
	// Header first (API clients).
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}

	// Cookie fallback (browser sessions).
	if c, err := r.Cookie("access_token"); err == nil && c.Value != "" {
		return c.Value
	}

	return ""
}

// tryRefresh attempts silent token refresh using cookies.
// On success, sets new cookies on the response.
func tryRefresh(w http.ResponseWriter, r *http.Request, svc *session.Service) (*identity.Identity, error) {
	sessionCookie, err := r.Cookie("session_id")
	if err != nil || sessionCookie.Value == "" {
		return nil, err
	}
	refreshCookie, err := r.Cookie("refresh_token")
	if err != nil || refreshCookie.Value == "" {
		return nil, err
	}

	pair, id, err := svc.Refresh(r.Context(), sessionCookie.Value, refreshCookie.Value)
	if err != nil {
		// Clear stale cookies.
		clearAuthCookies(w)
		return nil, err
	}

	// Set rotated tokens.
	http.SetCookie(w, &http.Cookie{
		Name:     "access_token",
		Value:    pair.AccessToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    pair.RefreshToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    id.SessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	return id, nil
}

// isCookieBased returns true if the access token came from a cookie, not Authorization header.
func isCookieBased(r *http.Request) bool {
	return r.Header.Get("Authorization") == ""
}

func clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{"access_token", "refresh_token", "session_id"} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
		})
	}
}
