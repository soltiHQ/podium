package responder

import (
	"encoding/json"
	"net/http"
)

// JSONResponder writes JSON HTTP responses.
type JSONResponder struct{}

// NewJSON creates a new JSONResponder.
func NewJSON() *JSONResponder { return &JSONResponder{} }

// Respond writes v.Data as JSON with the provided HTTP status code.
func (x *JSONResponder) Respond(w http.ResponseWriter, r *http.Request, code int, v *View) {
	if v == nil || v.Data == nil {
		x.writeHeaders(w, code)
		return
	}
	buf, err := json.Marshal(v.Data)
	if err != nil {
		x.writeHeaders(w, http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"response encoding error"}`))
		return
	}
	x.writeHeaders(w, code)
	_, _ = w.Write(buf)
}

func (x *JSONResponder) writeHeaders(w http.ResponseWriter, code int) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")

	w.WriteHeader(code)
}
