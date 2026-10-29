package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/soltiHQ/control-plane/internal/auth"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// Auth returns middleware that verifies the access token and stores the identity in context.
// A browser whose access token is missing or expired is refreshed from its cookies. Missing
// or rejected credentials answer 401, credentials that could not be checked 503. secure
// decides whether the auth cookies Auth sets or clears carry the Secure attribute.
func Auth(verifier token.Verifier, sessionSvc *session.Service, secure func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mode := httpctx.ModeFromRequest(r)
			id, err := authenticate(w, r, verifier, sessionSvc, secure)
			switch {
			case err != nil:
				response.Unavailable(w, r, mode)
			case id == nil:
				response.Unauthorized(w, r, mode)
			default:
				next.ServeHTTP(w, r.WithContext(transportctx.WithIdentity(r.Context(), id)))
			}
		})
	}
}

// OptionalAuth is Auth for a route that also serves callers without valid
// credentials: they reach next without an identity. Credentials that could
// not be checked still answer 503.
func OptionalAuth(verifier token.Verifier, sessionSvc *session.Service, secure func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := authenticate(w, r, verifier, sessionSvc, secure)
			switch {
			case err != nil:
				response.Unavailable(w, r, httpctx.ModeFromRequest(r))
			case id == nil:
				next.ServeHTTP(w, r)
			default:
				next.ServeHTTP(w, r.WithContext(transportctx.WithIdentity(r.Context(), id)))
			}
		})
	}
}

// authenticate returns the identity of r, from its bearer token or access
// cookie, or else, for a browser, from a refresh with its refresh and session
// cookies. There are three outcomes:
//
//   - valid credentials: the identity;
//   - no usable credentials (missing, malformed, expired, revoked or unknown,
//     or a user without permissions): neither identity nor error. A rejected
//     access cookie clears the auth cookies; a rejected refresh writes none;
//   - credentials that could not be checked, such as during a storage outage
//     in the access check or the refresh: the error. No cookie is written.
func authenticate(w http.ResponseWriter, r *http.Request, verifier token.Verifier, sessionSvc *session.Service, secure func(*http.Request) bool) (*identity.Identity, error) {
	raw, fromHeader := extractBearer(r)
	if raw != "" {
		id, err := verifier.Verify(r.Context(), raw)
		if err == nil {
			id, err = sessionSvc.AuthorizeAccess(r.Context(), id)
			switch {
			case err == nil:
				return id, nil
			case rejected(err):
				if !fromHeader {
					httpcookie.DeleteAuth(w, secure(r))
				}
				return nil, nil
			default:
				return nil, err
			}
		}
	}
	if fromHeader {
		return nil, nil
	}
	id, err := tryRefresh(w, r, sessionSvc, secure)
	switch {
	case err == nil:
		return id, nil
	case rejected(err):
		return nil, nil
	default:
		return nil, err
	}
}

// rejected reports whether err is the session service's verdict on the
// credentials, not a failure to check them.
func rejected(err error) bool {
	return errors.Is(err, auth.ErrInvalidRefresh) || errors.Is(err, auth.ErrRevoked) ||
		errors.Is(err, auth.ErrInvalidToken) || errors.Is(err, auth.ErrUnauthorized)
}

// RequirePermission returns middleware that checks identity for a specific permission.
func RequirePermission(perm enum.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var (
				mode   = httpctx.ModeFromRequest(r)
				id, ok = transportctx.Identity(r.Context())
			)
			if !ok {
				response.Unauthorized(w, r, mode)
				return
			}
			if !id.HasPermission(perm) {
				response.Forbidden(w, r, mode)
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
		return "", true
	}
	if c, err := httpcookie.GetAccessToken(r); err == nil && c.Value != "" {
		return c.Value, false
	}
	return "", false
}

func tryRefresh(w http.ResponseWriter, r *http.Request, svc *session.Service, secure func(*http.Request) bool) (*identity.Identity, error) {
	refreshCookie, err := httpcookie.GetRefreshToken(r)
	if err != nil || refreshCookie.Value == "" {
		return nil, auth.ErrInvalidRefresh
	}
	sessionCookie, err := httpcookie.GetSessionID(r)
	if err != nil || sessionCookie.Value == "" {
		return nil, auth.ErrInvalidRefresh
	}
	if svc == nil {
		return nil, auth.ErrInvalidRequest
	}

	pair, id, err := svc.Refresh(r.Context(), sessionCookie.Value, refreshCookie.Value)
	if err != nil {
		// A parallel rotation may already have installed newer browser cookies.
		// Reject this request without overwriting that successful response.
		return nil, err
	}
	id, err = svc.AuthorizeAccess(r.Context(), id)
	if err != nil {
		return nil, err
	}
	httpcookie.SetAuth(w, secure(r), pair.AccessToken, pair.RefreshToken, id.SessionID)
	return id, nil
}
