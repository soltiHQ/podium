package middleware

import (
	"net/http"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/transport/http/cookie"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// Logger returns middleware that logs every completed request.
func Logger(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			var (
				m   = httpsnoop.CaptureMetrics(next, w, r)
				evt = logger.Info()
			)
			if m.Code >= 500 {
				evt = logger.Error()
			} else if m.Code >= 400 {
				evt = logger.Warn()
			}

			evt.
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", m.Code).
				Int64("bytes", m.Written).
				Dur("duration", time.Since(start)).
				Str("remote", r.RemoteAddr)

			if rid, ok := transportctx.RequestID(r.Context()); ok {
				evt = evt.Str("request_id", rid)
			}
			if ua := r.UserAgent(); ua != "" {
				evt = evt.Str("user_agent", ua)
			}
			if sid, err := cookie.GetSessionID(r); err == nil {
				evt = evt.Str("session_id", sid.Value)
			}

			evt.Msg("http request")
		})
	}
}
