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

// JSON writes a JSON response with the given status code and payload.
// Returns an error if encoding fails (for logging purposes).
func JSON(w http.ResponseWriter, status int, data any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if data == nil {
		return nil
	}
	return json.NewEncoder(w).Encode(data)
}

// Error writes an ErrorResponse with the given status code.
// Automatically includes request ID from context if available.
func Error(ctx context.Context, w http.ResponseWriter, status int, message string) error {
	resp := ErrorResponse{
		Code:    status,
		Message: message,
	}
	if reqID, ok := transportctx.RequestID(ctx); ok {
		resp.RequestID = reqID
	}
	return JSON(w, status, resp)
}

// OK writes a 200 OK JSON response.
func OK(_ context.Context, w http.ResponseWriter, data any) error {
	return JSON(w, http.StatusOK, data)
}

// Created writes a 201 Created JSON response.
func Created(_ context.Context, w http.ResponseWriter, data any) error {
	return JSON(w, http.StatusCreated, data)
}

// NoContent writes a 204 No Content response without a body.
func NoContent(_ context.Context, w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

// BadRequest writes a 400 Bad Request error.
func BadRequest(ctx context.Context, w http.ResponseWriter, message string) error {
	return Error(ctx, w, http.StatusBadRequest, message)
}

// NotAllowed writes a 405 Method Not Allowed error.
func NotAllowed(ctx context.Context, w http.ResponseWriter, message string) error {
	return Error(ctx, w, http.StatusMethodNotAllowed, message)
}

// Unauthorized writes a 401 Unauthorized error.
func Unauthorized(ctx context.Context, w http.ResponseWriter, message string) error {
	return Error(ctx, w, http.StatusUnauthorized, message)
}

// Forbidden writes a 403 Forbidden error.
func Forbidden(ctx context.Context, w http.ResponseWriter, message string) error {
	return Error(ctx, w, http.StatusForbidden, message)
}

// NotFound writes a 404 Not Found error.
func NotFound(ctx context.Context, w http.ResponseWriter, message string) error {
	return Error(ctx, w, http.StatusNotFound, message)
}

// Conflict writes a 409 Conflict error.
func Conflict(ctx context.Context, w http.ResponseWriter, message string) error {
	return Error(ctx, w, http.StatusConflict, message)
}

// InternalError writes a 500 Internal Server Error.
func InternalError(ctx context.Context, w http.ResponseWriter, message string) error {
	return Error(ctx, w, http.StatusInternalServerError, message)
}
