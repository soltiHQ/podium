package status

import (
	"context"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// FromError maps a domain error to a gRPC status.
// Unknown errors are mapped to codes.Internal with a generic message
// to avoid leaking implementation details to clients.
func FromError(ctx context.Context, err error) *grpcstatus.Status {
	if err == nil {
		return grpcstatus.New(codes.OK, "")
	}

	code, msg := mapError(err)
	st := grpcstatus.New(code, msg)

	// Attach request ID as error detail when available.
	if rid, ok := transportctx.RequestID(ctx); ok {
		if detailed, detailErr := withRequestID(st, rid); detailErr == nil {
			st = detailed
		}
	}

	return st
}

// Errorf creates a gRPC status error with explicit code and message.
// Use when the caller knows the exact code (e.g. validation in handlers).
func Errorf(ctx context.Context, code codes.Code, format string, args ...any) error {
	st := grpcstatus.Newf(code, format, args...)

	if rid, ok := transportctx.RequestID(ctx); ok {
		if detailed, err := withRequestID(st, rid); err == nil {
			st = detailed
		}
	}

	return st.Err()
}

func mapError(err error) (codes.Code, string) {
	switch {
	// Context errors — first, most specific.
	case errors.Is(err, context.Canceled):
		return codes.Canceled, "request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded, "deadline exceeded"

	// Auth errors.
	case errors.Is(err, auth.ErrInvalidCredentials),
		errors.Is(err, auth.ErrPasswordMismatch),
		errors.Is(err, auth.ErrInvalidToken),
		errors.Is(err, auth.ErrExpiredToken),
		errors.Is(err, auth.ErrInvalidRefresh),
		errors.Is(err, auth.ErrRevoked):
		return codes.Unauthenticated, "unauthenticated"

	case errors.Is(err, auth.ErrUnauthorized):
		return codes.PermissionDenied, "permission denied"

	case errors.Is(err, auth.ErrInvalidRequest),
		errors.Is(err, auth.ErrInvalidArgument),
		errors.Is(err, auth.ErrWrongAuthKind):
		return codes.InvalidArgument, "invalid argument"

	// Storage errors.
	case errors.Is(err, storage.ErrNotFound):
		return codes.NotFound, "not found"
	case errors.Is(err, storage.ErrAlreadyExists):
		return codes.AlreadyExists, "already exists"
	case errors.Is(err, storage.ErrConflict):
		return codes.Aborted, "conflict"
	case errors.Is(err, storage.ErrInvalidArgument):
		return codes.InvalidArgument, "invalid argument"

	// Everything else — internal, no detail leak.
	default:
		return codes.Internal, "internal error"
	}
}

// withRequestID attaches a request ID to the gRPC status as RequestInfo detail.
func withRequestID(st *grpcstatus.Status, rid string) (*grpcstatus.Status, error) {
	return st.WithDetails(&errdetails.RequestInfo{
		RequestId: rid,
	})
}
