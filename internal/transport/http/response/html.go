package response

import (
	"net/http"

	"github.com/a-h/templ"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

const defaultLoginPath = "/login"

// HTMLConfig controls HTMLResponder behavior.
type HTMLConfig struct {
	// LoginPath is the redirect target for 401 errors.
	// Defaults to "/login".
	LoginPath string

	// ErrorPage is a templ component factory for error pages.
	// Receives code, message, and request ID.
	ErrorPage func(code int, title, message string) templ.Component
}

func (c HTMLConfig) withDefaults() HTMLConfig {
	if c.LoginPath == "" {
		c.LoginPath = defaultLoginPath
	}
	return c
}

// HTMLResponder renders responses using templ components.
type HTMLResponder struct {
	cfg HTMLConfig
}

// NewHTML creates an HTMLResponder.
func NewHTML(cfg HTMLConfig) *HTMLResponder {
	cfg = cfg.withDefaults()
	return &HTMLResponder{cfg: cfg}
}

// Respond renders a templ component from v.Component.
// If v is nil or v.Component is nil, writes only the status code.
func (h *HTMLResponder) Respond(w http.ResponseWriter, r *http.Request, code int, v *View) {
	if v == nil || v.Component == nil {
		w.WriteHeader(code)
		return
	}
	h.render(w, r, code, v.Component)
}

// Error renders an error page or redirects to login for 401.
func (h *HTMLResponder) Error(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if code == http.StatusUnauthorized {
		target := h.cfg.LoginPath + "?redirect=" + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	if h.cfg.ErrorPage == nil {
		http.Error(w, msg, code)
		return
	}

	rid, _ := transportctx.RequestID(r.Context())
	h.render(w, r, code, h.cfg.ErrorPage(code, msg, rid))
}

func (h *HTMLResponder) render(w http.ResponseWriter, r *http.Request, code int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := c.Render(r.Context(), w); err != nil {
		_, _ = w.Write([]byte("<!-- render error -->"))
	}
}
