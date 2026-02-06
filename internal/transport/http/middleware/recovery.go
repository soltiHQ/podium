package middleware

import (
	"net/http"
	"runtime/debug"
	"sync/atomic"

	"github.com/felixge/httpsnoop"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
)

type RecoveryConfig struct {
	OnPanic func(r *http.Request, recovered any, stack []byte)
}

// Recovery recovers panics from downstream handlers/middleware and returns 500.
//
// Response format is negotiated via response.InternalError:
//   - HTMX => HTML fragment
//   - Accept: text/html => HTML page
//   - otherwise => JSON
//
// Put it early in the chain (outermost), ideally right after RequestID/Trace.
func Recovery(cfg RecoveryConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var wrote atomic.Bool

			ww := httpsnoop.Wrap(w, httpsnoop.Hooks{
				WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
					return func(code int) {
						wrote.Store(true)
						next(code)
					}
				},
				Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
					return func(p []byte) (int, error) {
						// Write implies headers committed.
						wrote.Store(true)
						return next(p)
					}
				},
			})

			defer func() {
				rec := recover()
				if rec == nil {
					return
				}

				stack := debug.Stack()
				if cfg.OnPanic != nil {
					cfg.OnPanic(r, rec, stack)
				}

				// If response already started, cannot reliably change status/body.
				if wrote.Load() {
					return
				}

				_ = response.InternalError(r.Context(), ww, r, "internal server error")
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
