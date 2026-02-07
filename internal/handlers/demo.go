package handlers

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transport/http/response"
)

// Demo is a demo handler for smoke testing.
type Demo struct {
	json *response.JSONResponder
}

// NewDemo creates a demo handler.
func NewDemo(json *response.JSONResponder) *Demo {
	return &Demo{json: json}
}

// Hello returns a greeting.
func (d *Demo) Hello(w http.ResponseWriter, r *http.Request) {
	d.json.Respond(w, r, http.StatusOK, &response.View{
		Data: map[string]string{"message": "hello"},
	})
}

// Routes registers demo routes on the given mux.
func (d *Demo) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/hello", d.Hello)
}
