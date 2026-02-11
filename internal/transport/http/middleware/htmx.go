package middleware

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transport/http/response"
)

func RequireHTMX(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("HX-Request") != "true" {
			response.BadRequest(w, r, response.RenderPage)
			return
		}

		next.ServeHTTP(w, r)
	})
}
