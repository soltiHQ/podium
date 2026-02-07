package response

import (
	"net/http"
	"strings"
)

// Negotiate returns middleware that picks the appropriate Responder per request and stores it in context.
func Negotiate(json *JSONResponder, html *HTMLResponder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := pick(r, json, html)
			ctx := WithResponder(r.Context(), resp)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func pick(r *http.Request, j *JSONResponder, h *HTMLResponder) Responder {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return j
	}
	if r.Header.Get("HX-Request") == "true" {
		return h
	}

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		return j
	}
	if strings.Contains(accept, "text/html") {
		return h
	}
	return j
}
