package response

import (
	"html/template"
	"io/fs"
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

const (
	defaultErrorTemplate = "error.html"
	defaultLoginPath     = "/login"
)

// HTMLConfig controls HTMLResponder behavior.
type HTMLConfig struct {
	// LoginPath is the redirect target for 401 errors.
	// Defaults to "/login".
	LoginPath string

	// ErrorTemplate is the template name for error pages.
	// Defaults to "error.html".
	ErrorTemplate string
}

func (c HTMLConfig) withDefaults() HTMLConfig {
	if c.LoginPath == "" {
		c.LoginPath = defaultLoginPath
	}
	if c.ErrorTemplate == "" {
		c.ErrorTemplate = defaultErrorTemplate
	}
	return c
}

// HTMLResponder renders HTML responses using Go templates.
type HTMLResponder struct {
	templates *template.Template
	cfg       HTMLConfig
}

// NewHTML creates an HTMLResponder.
func NewHTML(fsys fs.FS, cfg HTMLConfig) (*HTMLResponder, error) {
	tmpl, err := template.ParseFS(fsys, "templates/*.html", "templates/**/*.html")
	if err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	return &HTMLResponder{
		templates: tmpl,
		cfg:       cfg,
	}, nil
}

// Respond renders the template specified in v.Template with v.Data.
func (h *HTMLResponder) Respond(w http.ResponseWriter, r *http.Request, code int, v *View) {
	if v == nil || v.Template == "" {
		w.WriteHeader(code)
		return
	}
	h.render(w, r, code, v.Template, v.Data)
}

// Error renders an error page or redirects to log in for 401.
func (h *HTMLResponder) Error(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if code == http.StatusUnauthorized {
		target := h.cfg.LoginPath + "?redirect=" + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	data := map[string]any{
		"Code":    code,
		"Message": msg,
	}
	if rid, ok := transportctx.RequestID(r.Context()); ok {
		data["RequestID"] = rid
	}
	h.render(w, r, code, h.cfg.ErrorTemplate, data)
}

func (h *HTMLResponder) render(w http.ResponseWriter, _ *http.Request, code int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		_, _ = w.Write([]byte("<!-- template error -->"))
	}
}

// isHTMX reports whether the request was made by HTMX.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
