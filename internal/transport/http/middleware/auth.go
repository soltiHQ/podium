package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/cluster"

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
// or rejected credentials answer 401, credentials that could not be checked 503, after an
// ERROR log line with the method, the path and the error, never a credential. A refresh that
// needs the leader on a follower is no such failure: see checkFailed. secure
// decides whether the auth cookies Auth sets or clears carry the Secure attribute.
func Auth(verifier token.Verifier, sessionSvc *session.Service, secure func(*http.Request) bool, logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mode := httpctx.ModeFromRequest(r)
			id, err := authenticate(w, r, verifier, sessionSvc, secure)
			switch {
			case err != nil:
				checkFailed(w, r, mode, logger, err)
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
// not be checked still answer 503, logged like Auth.
func OptionalAuth(verifier token.Verifier, sessionSvc *session.Service, secure func(*http.Request) bool, logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := authenticate(w, r, verifier, sessionSvc, secure)
			switch {
			case err != nil:
				checkFailed(w, r, httpctx.ModeFromRequest(r), logger, err)
			case id == nil:
				next.ServeHTTP(w, r)
			default:
				next.ServeHTTP(w, r.WithContext(transportctx.WithIdentity(r.Context(), id)))
			}
		})
	}
}

// checkFailed answers 503 to a request whose credentials could not be checked.
// It logs why at ERROR: the method, the path and the error, never a cookie or a
// token. The exception is a refresh that met the leadership
// (cluster.IsLeadershipError), which only a refresh with auth.rotate_refresh
// on can, as the rotation of the refresh token is a write. That is the
// deployment's routing, not a failure of this node, so it is logged at WARN,
// worded for what happened, and answered by response.FromError: the 503
// carries the leader's address in X-Leader when this node knows the leader as
// it answers (httpctx.Leader), and on an API route, whose access log (Logger)
// wraps Auth, that header makes the access log line a WARN as well. A node
// that knows no leader answers the 503 without X-Leader, which that access log
// logs at ERROR. A node that shut down is one: hashicorp/raft clears the known
// leader at its shutdown.
//
//   - cluster.ErrNotLeader: the node does not lead, as every follower and a
//     node that shut down, and could not rotate the token, so nothing changed;
//   - cluster.ErrLeadershipLost: the node lost its leadership, or shut down,
//     while the rotation was committing, which may have taken effect. A node
//     that lost its leadership names the new leader when it knows it by then: a
//     retry there refreshes the browser when the rotation did not commit, and
//     when it did, the browser's refresh token no longer matches, and it logs
//     in again. A node that shut down names no leader.
func checkFailed(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, logger zerolog.Logger, err error) {
	if cluster.IsLeadershipError(err) {
		message := "auth refresh needs the leader: with auth.rotate_refresh only the leader rotates a refresh token, and this node does not lead, as a follower or a node that shut down, so nothing changed: route browsers to the leader, which the 503 names in X-Leader when this node knows it, which a node that shut down does not, or turn rotation off"
		if errors.Is(err, cluster.ErrLeadershipLost) {
			message = "auth refresh outcome unknown: with auth.rotate_refresh, this node lost its leadership, or shut down, while the rotation of the refresh token committed, which may have taken effect: a retry at the current leader refreshes the browser unless it did, and then the browser logs in again; the 503 names that leader in X-Leader when this node knows it, which a node that shut down does not"
		}
		logger.Warn().Err(err).Str("method", r.Method).Str("path", logPath(r.URL.Path)).Msg(message)
		response.FromError(w, r, mode, err)
		return
	}
	logCheckFailure(logger, r, err)
	response.Unavailable(w, r, mode)
}

// logCheckFailure logs why credentials could not be checked: the method, the
// path through logPath, and the error; never a cookie, a token or a session ID.
func logCheckFailure(logger zerolog.Logger, r *http.Request, err error) {
	logger.Error().Err(err).Str("method", r.Method).Str("path", logPath(r.URL.Path)).Msg("auth check failed")
}

// authenticate returns the identity of r, from its bearer token or access
// cookie, or else, for a browser, from a refresh with its refresh and session
// cookies. There are three outcomes:
//
//   - valid credentials: the identity;
//   - no usable credentials (missing, malformed, expired, revoked or unknown,
//     or a user without permissions): neither identity nor error. An access
//     cookie that does not verify (expired, malformed or badly signed) falls
//     through to the refresh. Only an access cookie that verifies but that
//     the session check rejects (a session that is revoked, expired or
//     unknown, a user that is disabled or deleted, or no permissions) clears
//     the auth cookies; a rejected refresh writes none;
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

// Recheck returns the access check Auth makes when a request starts, followed
// by RequirePermission(perm) when perm is set, for a request that is still
// running: the session of the identity Auth stored in its context must still
// be live and its user enabled, with some permission and, when perm is set,
// with perm. A long-lived stream calls it every keepalive interval and ends on
// any error. Credentials that could not be checked, during a storage outage for
// one, are an error too, logged like in Auth.
func Recheck(sessionSvc *session.Service, perm enum.Permission, logger zerolog.Logger) func(*http.Request) error {
	return func(r *http.Request) error {
		id, ok := transportctx.Identity(r.Context())
		if !ok {
			return auth.ErrUnauthorized
		}
		current, err := sessionSvc.AuthorizeAccess(r.Context(), id)
		switch {
		case err == nil:
		case rejected(err):
			return err
		default:
			logCheckFailure(logger, r, err)
			return err
		}
		if perm != "" && !current.HasPermission(perm) {
			return auth.ErrUnauthorized
		}
		return nil
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
