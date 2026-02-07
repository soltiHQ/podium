package response

import "net/http"

// Responder writes HTTP responses in a format appropriate for the client.
// Implementations: JSON (API clients), HTML (browsers, HTMX).
//
// Middleware and format-agnostic handlers use Responder from context.
// Format-specific handlers may use JSONResponder or HTMLResponder directly.
type Responder interface {
	// Respond writes a success response.
	// JSON: serializes v.Data as body.
	// HTML: renders v.Template with v.Data.
	Respond(w http.ResponseWriter, r *http.Request, code int, v *View)

	// Error writes an error response.
	// JSON: writes ErrorBody as JSON.
	// HTML: redirects to login (401) or renders the error page.
	Error(w http.ResponseWriter, r *http.Request, code int, msg string)
}

// View carries response data in a format-agnostic way.
type View struct {
	// Template is the HTML template name (e.g. "agents/list.html").
	// Ignored by JSONResponder.
	Template string

	// Data is the response payload.
	// JSON: serialized as response body.
	// HTML: passed to template as dot context.
	Data any
}
