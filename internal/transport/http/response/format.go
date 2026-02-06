package response

import (
	"net/http"
	"strings"
)

const (
	HeaderAccept    = "Accept"
	HeaderHXRequest = "HX-Request"
)

// IsHTMX reports whether the request was sent by htmx.
func IsHTMX(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(HeaderHXRequest)), "true")
}

// WantsHTML reports whether the client prefers HTML.
func WantsHTML(r *http.Request) bool {
	if r == nil {
		return false
	}
	if IsHTMX(r) {
		return true
	}

	accept := r.Header.Get(HeaderAccept)
	accept = strings.ToLower(accept)

	if accept == "" {
		return false
	}
	if strings.Contains(accept, "text/html") || strings.Contains(accept, "application/xhtml+xml") {
		return true
	}
	return false
}

// WantsJSON reports whether the client prefers JSON.
func WantsJSON(r *http.Request) bool {
	if r == nil {
		return true
	}
	if IsHTMX(r) {
		return false
	}
	accept := strings.TrimSpace(r.Header.Get(HeaderAccept))
	if accept == "" {
		return true
	}
	accept = strings.ToLower(accept)
	return strings.Contains(accept, "application/json") || strings.Contains(accept, "*/*")
}
