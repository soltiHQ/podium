package responder

import (
	"net/http"

	"github.com/a-h/templ"
)

// HTMLResponder renders responses using templ components.
type HTMLResponder struct{}

// NewHTML creates an HTMLResponder.
func NewHTML() *HTMLResponder { return &HTMLResponder{} }

// Respond renders a templ component from v.Component.
func (x *HTMLResponder) Respond(w http.ResponseWriter, r *http.Request, code int, v *View) {
	if code == http.StatusUnauthorized {
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			x.writeHeaders(w, http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	x.writeHeaders(w, code)
	if v == nil || v.Component == nil {
		return
	}
	x.render(w, r, code, v.Component)
}

func (x *HTMLResponder) writeHeaders(w http.ResponseWriter, code int) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")

	w.WriteHeader(code)
}

func (x *HTMLResponder) render(w http.ResponseWriter, r *http.Request, _ int, c templ.Component) {
	if err := c.Render(r.Context(), w); err != nil {
		_, _ = w.Write([]byte("<!-- render error -->"))
	}
}
