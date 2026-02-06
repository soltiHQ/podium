package response

import (
	"errors"

	"github.com/soltiHQ/control-plane/internal/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FromError maps application/storage/auth errors to gRPC status errors.
// If err is nil, returns nil.
func FromError(err error) error {
	if err == nil {
		return nil
	}

	switch {
	// STORAGE
	case errors.Is(err, storage.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, "invalid argument")

	case errors.Is(err, storage.ErrNotFound):
		return status.Error(codes.NotFound, "not found")

	case errors.Is(err, storage.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, "already exists")

	case errors.Is(err, storage.ErrConflict):
		// Some systems prefer Aborted / FailedPrecondition; choose one and stick with it.
		return status.Error(codes.Aborted, "conflict")

	// DEFAULT
	default:
		return status.Error(codes.Internal, "internal error")
	}
}
