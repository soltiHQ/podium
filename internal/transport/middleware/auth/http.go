package auth

import (
	"net/http"

	authcore "github.com/soltiHQ/control-plane/auth"
	"github.com/soltiHQ/control-plane/internal/transport/response"
	"github.com/soltiHQ/control-plane/internal/transportctx"

	"github.com/rs/zerolog"
)

// HTTP returns an HTTP middleware that authenticates requests using the provided verifier.
// On success, the authenticated identity is stored in the request context.
// On failure, a JSON error response is written and the request is aborted.
func HTTP(verifier authcore.Verifier, logger zerolog.Logger) func(http.Handler) http.Handler {
	l := logger.With().Str("middleware", "auth_http").Logger()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			rawAuth := r.Header.Get("Authorization")
			token := extractBearerToken(rawAuth)
			if token == "" {
				ev := l.Warn().
					Str("method", r.Method).
					Str("path", r.URL.Path).
					Str("reason", "missing or malformed Authorization header")

				if reqID, ok := transportctx.RequestID(r.Context()); ok {
					ev = ev.Str("request_id", reqID)
				}
				ev.Msg("auth: unauthorized request")

				_ = response.Unauthorized(r.Context(), w, "unauthorized")
				return
			}

			id, err := verifier.Verify(r.Context(), token)
			if err != nil {
				statusCode, msg := httpErrorFromAuth(err)

				ev := l.Warn().
					Err(err).
					Str("method", r.Method).
					Str("path", r.URL.Path).
					Int("status", statusCode)

				if reqID, ok := transportctx.RequestID(r.Context()); ok {
					ev = ev.Str("request_id", reqID)
				}
				if id != nil && id.Subject != "" {
					ev = ev.Str("subject", id.Subject)
				}
				ev.Msg("auth: verification failed")

				switch statusCode {
				case http.StatusUnauthorized:
					_ = response.Unauthorized(r.Context(), w, msg)
				case http.StatusForbidden:
					_ = response.Forbidden(r.Context(), w, msg)
				default:
					_ = response.Unauthorized(r.Context(), w, msg)
				}
				return
			}

			ctx := transportctx.WithIdentity(r.Context(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
