package middleware

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
)

// CrossOrigin returns middleware that refuses state-changing requests a browser
// sends from another origin (http.CrossOriginProtection) with 403 "Cross-origin
// request blocked.". The answer goes through the request's Responder, so the
// middleware runs inside Negotiate.
//
// GET, HEAD and OPTIONS always pass, as do requests without Sec-Fetch-Site or
// Origin (non-browser clients). The exact origins of cfg.AllowOrigins are
// trusted; "*" trusts none. It panics on an origin cfg.Validate rejects.
func CrossOrigin(cfg CORSConfig) func(http.Handler) http.Handler {
	p, err := cfg.crossOriginProtection()
	if err != nil {
		panic("middleware.CrossOrigin: " + err.Error())
	}
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response.ForbiddenMsg(w, r, httpctx.ModeFromRequest(r), "Cross-origin request blocked.")
	}))
	return p.Handler
}
