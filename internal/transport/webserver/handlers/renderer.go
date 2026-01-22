package handlers

import "net/http"

// Renderer abstracts template rendering for HTTP handlers.
type Renderer interface {
	Render(w http.ResponseWriter, name string, data any) error
}
