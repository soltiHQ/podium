package responder

import (
	"net/http"

	"github.com/a-h/templ"
)

// Responder writes HTTP responses in a format appropriate for the client.
type Responder interface {
	Respond(w http.ResponseWriter, r *http.Request, code int, v *View)
}

// View carries response data in a format-agnostic way.
type View struct {
	// Component is a templ component for HTML rendering.
	// Ignored by JSONResponder.
	Component templ.Component
	// Data is the response payload for JSON rendering.
	// Ignored by HTMLResponder.
	Data any
}
