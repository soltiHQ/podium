package response

import (
	"context"
	"errors"
	"net/http"

	"github.com/soltiHQ/control-plane/auth"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// FromError maps a domain/auth/storage error to an HTTP response.
func FromError(ctx context.Context, w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}

	switch {
	// AUTH
	case errors.Is(err, auth.ErrInvalidToken),
		errors.Is(err, auth.ErrExpiredToken):
		_ = Unauthorized(ctx, w, "invalid or expired token")
		return true

	case errors.Is(err, auth.ErrUnauthorized):
		_ = Forbidden(ctx, w, "insufficient permissions")
		return true

	// STORAGE
	case errors.Is(err, storage.ErrInvalidArgument):
		_ = BadRequest(ctx, w, "invalid request parameters")
		return true

	case errors.Is(err, storage.ErrNotFound):
		_ = NotFound(ctx, w, "resource not found")
		return true

	case errors.Is(err, storage.ErrConflict):
		_ = Conflict(ctx, w, "resource conflict")
		return true

	// DEFAULT
	default:
		_ = InternalError(ctx, w, "internal server error")
		return true
	}
}
