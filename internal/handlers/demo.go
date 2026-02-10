package handlers

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/ui/pages"
)

// Demo is a demo handler for smoke testing.
type Demo struct {
	json *responder.JSONResponder
}

// NewDemo creates a demo handler.
func NewDemo(json *responder.JSONResponder) *Demo {
	return &Demo{json: json}
}

// Hello returns a greeting.
func (d *Demo) Hello(w http.ResponseWriter, r *http.Request) {
	response.OK(w, r, response.RenderPage, &responder.View{
		Data: map[string]string{"message": "hello"},
		Component: pages.ErrorPage(
			http.StatusOK,
			"Page",
			"Hello world",
			"reqId",
		),
	})
}

// Routes registers demo routes on the given mux.
func (d *Demo) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/hello", d.Hello)
	mux.HandleFunc("GET /hello", d.Hello)
}
