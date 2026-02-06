package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

type TokenSource int

const (
	TokenSourceAuthorizationHeader TokenSource = iota
	TokenSourceCookie
)

type AuthnConfig struct {
	Verifier token.Verifier
	Store    storage.Storage // needs SessionStore inside
	Clock    func() time.Time

	// Where to read token from.
	// Default: Authorization header only.
	Sources []TokenSource

	// CookieName is used when TokenSourceCookie is enabled.
	// Default: "access_token".
	CookieName string

	// AllowAnonymous allows requests without token to pass through.
	// When false, missing/invalid token => 401.
	AllowAnonymous bool
}

// Authn verifies access token, validates session (revoke/ttl), and sets identity in transportctx.
func Authn(cfg AuthnConfig) func(http.Handler) http.Handler {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = []TokenSource{TokenSourceAuthorizationHeader}
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "access_token"
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.Verifier == nil || cfg.Store == nil {
				_ = response.InternalError(r.Context(), w, r, "internal server error")
				return
			}

			raw := extractToken(r, cfg.Sources, cfg.CookieName)
			if raw == "" {
				if cfg.AllowAnonymous {
					next.ServeHTTP(w, r)
					return
				}
				_ = response.Unauthorized(r.Context(), w, r, "missing access token")
				return
			}

			id, err := cfg.Verifier.Verify(r.Context(), raw)
			if err != nil {
				// token-level errors -> 401
				if errors.Is(err, token.ErrInvalidToken) || errors.Is(err, token.ErrExpiredToken) {
					_ = response.Unauthorized(r.Context(), w, r, "invalid or expired token")
					return
				}
				// everything else is internal (verifier misconfig, etc)
				_ = response.InternalError(r.Context(), w, r, "internal server error")
				return
			}
			if id == nil {
				_ = response.Unauthorized(r.Context(), w, r, "invalid token")
				return
			}

			// Session checks (revoke + ttl).
			if err := validateSession(r.Context(), cfg, id); err != nil {
				switch {
				case errors.Is(err, storage.ErrNotFound):
					_ = response.Unauthorized(r.Context(), w, r, "session not found")
				case errors.Is(err, storage.ErrInvalidArgument):
					_ = response.Unauthorized(r.Context(), w, r, "invalid token")
				default:
					// revoked/expired => 401, unexpected => 500
					if errors.Is(err, errSessionInvalid) {
						_ = response.Unauthorized(r.Context(), w, r, "session invalid")
					} else {
						_ = response.InternalError(r.Context(), w, r, "internal server error")
					}
				}
				return
			}

			// attach identity to context
			ctx := transportctx.WithIdentity(r.Context(), id)
			r = r.WithContext(ctx)

			next.ServeHTTP(w, r)
		})
	}
}

var errSessionInvalid = errors.New("authn: session invalid")

func validateSession(ctx context.Context, cfg AuthnConfig, id *identity.Identity) error {
	// If you decide some tokens are session-less later (service tokens), gate it here.
	if id.SessionID == "" {
		return storage.ErrInvalidArgument
	}
	if id.UserID == "" {
		return storage.ErrInvalidArgument
	}

	sess, err := cfg.Store.GetSession(ctx, id.SessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return storage.ErrNotFound
	}

	// hard binding: token must belong to the same user as the session
	if sess.UserID() != id.UserID {
		return errSessionInvalid
	}

	now := cfg.Clock()

	if sess.Revoked() {
		return errSessionInvalid
	}
	if sess.Expired(now) {
		return errSessionInvalid
	}

	// Optional extra safety:
	// If session was created after token iat (clock skew), you can invalidate.
	// if !id.IssuedAt.IsZero() && sess.CreatedAt().After(id.IssuedAt.Add(30*time.Second)) { ... }

	return nil
}

func extractToken(r *http.Request, sources []TokenSource, cookieName string) string {
	for _, src := range sources {
		switch src {
		case TokenSourceAuthorizationHeader:
			if raw := tokenFromAuthorization(r); raw != "" {
				return raw
			}
		case TokenSourceCookie:
			if raw := tokenFromCookie(r, cookieName); raw != "" {
				return raw
			}
		}
	}
	return ""
}

func tokenFromAuthorization(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if v == "" {
		return ""
	}
	// Authorization: Bearer <token>
	const prefix = "bearer "
	if len(v) < len(prefix) {
		return ""
	}
	if !strings.EqualFold(v[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(v[len(prefix):])
}

func tokenFromCookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil || c == nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}
