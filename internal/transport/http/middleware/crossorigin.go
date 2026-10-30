package middleware

import (
	"net/http"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// CrossOrigin returns middleware that refuses state-changing requests a browser
// sends from another origin (http.CrossOriginProtection) with 403 "Cross-origin
// request blocked.". The answer goes through the request's Responder, so the
// middleware runs inside Negotiate. It also runs before any route, so a refused
// request gets no request ID and no access-log line: logger records each
// refusal at WARN instead.
//
// GET, HEAD and OPTIONS always pass, as do requests without Sec-Fetch-Site or
// Origin (non-browser clients). The exact origins of cfg.AllowOrigins are
// trusted; "*" trusts none. It panics on an origin cfg.Validate rejects.
func CrossOrigin(cfg CORSConfig, logger zerolog.Logger) func(http.Handler) http.Handler {
	p, err := cfg.crossOriginProtection()
	if err != nil {
		panic("middleware.CrossOrigin: " + err.Error())
	}
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Warn().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Str("sec_fetch_site", r.Header.Get("Sec-Fetch-Site")).
			Str("origin", r.Header.Get("Origin")).
			Msg("cross-origin request blocked")
		response.ForbiddenMsg(w, r, httpctx.ModeFromRequest(r), "Cross-origin request blocked.")
	}))
	return p.Handler
}
