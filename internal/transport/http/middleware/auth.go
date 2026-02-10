package middleware

import (
	"net/http"
	"strings"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/transport/http/cookie"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// Auth returns middleware that verifies the access token and stores the identity in context.
func Auth(verifier token.Verifier, sessionSvc *session.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, fromHeader := extractBearer(r)
			if raw == "" {
				response.Unauthorized(w, r, response.RenderPage)
				return
			}

			id, err := verifier.Verify(r.Context(), raw)
			if err == nil {
				next.ServeHTTP(w, r.WithContext(transportctx.WithIdentity(r.Context(), id)))
				return
			}
			if fromHeader {
				response.Unauthorized(w, r, response.RenderPage)
				return
			}

			id, err = tryRefresh(w, r, sessionSvc)
			if err != nil {
				response.Unauthorized(w, r, response.RenderPage)
				return
			}
			next.ServeHTTP(w, r.WithContext(transportctx.WithIdentity(r.Context(), id)))
		})
	}
}

// RequirePermission returns middleware that checks identity for a specific permission.
func RequirePermission(perm kind.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := transportctx.Identity(r.Context())
			if !ok {
				response.Unauthorized(w, r, response.RenderPage)
				return
			}
			if !id.HasPermission(perm) {
				response.Forbidden(w, r, response.RenderPage)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func extractBearer(r *http.Request) (token string, fromHeader bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):]), true
		}
	}
	if c, err := cookie.GetAccessToken(r); err == nil && c.Value != "" {
		return c.Value, false
	}
	return "", false
}

func tryRefresh(w http.ResponseWriter, r *http.Request, svc *session.Service) (*identity.Identity, error) {
	refreshCookie, err := cookie.GetRefreshToken(r)
	if err != nil || refreshCookie.Value == "" {
		return nil, err
	}
	sessionCookie, err := cookie.GetSessionID(r)
	if err != nil || sessionCookie.Value == "" {
		return nil, err
	}

	pair, id, err := svc.Refresh(r.Context(), sessionCookie.Value, refreshCookie.Value)
	if err != nil {
		cookie.DeleteAuth(w, r)
		return nil, err
	}
	cookie.SetAuth(w, r, pair.AccessToken, pair.RefreshToken, id.SessionID)
	return id, nil
}
