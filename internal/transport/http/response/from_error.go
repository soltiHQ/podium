package response

import (
	"context"
	"errors"
	"net/http"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// FromError maps an application error into an HTTP response.
// Returns true if it wrote a response.
func FromError(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}

	switch {
	// STORAGE
	case errors.Is(err, storage.ErrInvalidArgument):
		_ = BadRequest(ctx, w, r, "invalid request parameters")
		return true
	case errors.Is(err, storage.ErrNotFound):
		_ = NotFound(ctx, w, r, "resource not found")
		return true
	case errors.Is(err, storage.ErrConflict):
		_ = Conflict(ctx, w, r, "resource conflict")
		return true
	case errors.Is(err, storage.ErrAlreadyExists):
		_ = Conflict(ctx, w, r, "resource already exists")
		return true

	// DEFAULT
	default:
		_ = InternalError(ctx, w, r, "internal server error")
		return true
	}
}

// Convenience for places that only have request and error.
func FromRequestError(w http.ResponseWriter, r *http.Request, err error) bool {
	return FromError(r.Context(), w, r, err)
}

// Status helper for direct use in handlers.
func Status(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, message string) {
	_ = Error(ctx, w, r, status, message)
}

// RawStatus is useful for ultra-low-level places.
func RawStatus(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
}

// Redirect ..
func Redirect(w http.ResponseWriter, r *http.Request, location string, code int) {
	http.Redirect(w, r, location, code)
}
