package response

import (
	"context"
	"net/http"
)

func OK(_ context.Context, w http.ResponseWriter, data any) error {
	return writeJSON(w, http.StatusOK, data)
}

func Created(_ context.Context, w http.ResponseWriter, data any) error {
	return writeJSON(w, http.StatusCreated, data)
}

func NoContent(_ context.Context, w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

// Error writes an error response negotiated by request headers:
//   - HTMX => HTML fragment
//   - Accept: text/html => HTML page
//   - Otherwise => JSON
func Error(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, message string) error {
	switch {
	case IsHTMX(r):
		return writeHTMLFragment(ctx, w, status, message)
	case WantsHTML(r):
		return writeHTMLPage(ctx, w, status, message)
	default:
		return writeJSONError(ctx, w, status, message)
	}
}

func BadRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, message string) error {
	return Error(ctx, w, r, http.StatusBadRequest, message)
}

func Unauthorized(ctx context.Context, w http.ResponseWriter, r *http.Request, message string) error {
	return Error(ctx, w, r, http.StatusUnauthorized, message)
}

func Forbidden(ctx context.Context, w http.ResponseWriter, r *http.Request, message string) error {
	return Error(ctx, w, r, http.StatusForbidden, message)
}

func NotFound(ctx context.Context, w http.ResponseWriter, r *http.Request, message string) error {
	return Error(ctx, w, r, http.StatusNotFound, message)
}

func Conflict(ctx context.Context, w http.ResponseWriter, r *http.Request, message string) error {
	return Error(ctx, w, r, http.StatusConflict, message)
}

func InternalError(ctx context.Context, w http.ResponseWriter, r *http.Request, message string) error {
	return Error(ctx, w, r, http.StatusInternalServerError, message)
}

func NotAllowed(ctx context.Context, w http.ResponseWriter, r *http.Request, message string) error {
	return Error(ctx, w, r, http.StatusMethodNotAllowed, message)
}
