package response

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// ErrorResponse is an API-safe error envelope.
type ErrorResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if data == nil {
		return nil
	}
	return json.NewEncoder(w).Encode(data)
}

func writeJSONError(ctx context.Context, w http.ResponseWriter, status int, message string) error {
	resp := ErrorResponse{
		Code:    status,
		Message: message,
	}
	if reqID, ok := transportctx.RequestID(ctx); ok {
		resp.RequestID = reqID
	}
	return writeJSON(w, status, resp)
}
