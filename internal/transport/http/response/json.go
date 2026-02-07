package response

import (
	"encoding/json"
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// ErrorBody is the standard API error shape.
type ErrorBody struct {
	Code      int    `json:"code"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// JSONResponder writes JSON responses.
type JSONResponder struct{}

// NewJSON creates a JSONResponder.
func NewJSON() *JSONResponder { return &JSONResponder{} }

// Respond writes v.Data as a JSON body.
func (j *JSONResponder) Respond(w http.ResponseWriter, _ *http.Request, code int, v *View) {
	if v == nil || v.Data == nil {
		w.WriteHeader(code)
		return
	}
	j.encode(w, code, v.Data)
}

// Error writes an ErrorBody as JSON with the request ID from context.
func (j *JSONResponder) Error(w http.ResponseWriter, r *http.Request, code int, msg string) {
	body := ErrorBody{
		Code:    code,
		Message: msg,
	}
	if rid, ok := transportctx.RequestID(r.Context()); ok {
		body.RequestID = rid
	}
	j.encode(w, code, body)
}

// encode marshals data first to avoid partial writes on encoding errors.
func (j *JSONResponder) encode(w http.ResponseWriter, code int, data any) {
	buf, err := json.Marshal(data)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"response encoding error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(buf)
}
