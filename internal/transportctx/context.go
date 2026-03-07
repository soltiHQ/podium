// Package transportctx provides transport-agnostic context values shared by
// HTTP middleware, gRPC interceptors, handlers, and loggers.
package transportctx

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
)

type (
	identityKey  struct{}
	requestIDKey struct{}
	errorKey     struct{}
)

const unknownRequestID = "unknown"

// WithIdentity stores authenticated identity in ctx.
func WithIdentity(ctx context.Context, id *identity.Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// WithRequestID stores request id in ctx.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// Identity returns identity from ctx (if any).
func Identity(ctx context.Context) (*identity.Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(*identity.Identity)
	return id, ok && id != nil
}

// RequestID returns request id from ctx (if any).
func RequestID(ctx context.Context) (string, bool) {
	rid, ok := ctx.Value(requestIDKey{}).(string)
	return rid, ok && rid != ""
}

// TryRequestID returns request id from ctx (if any).
func TryRequestID(ctx context.Context) string {
	if rid, ok := RequestID(ctx); ok {
		return rid
	}
	return unknownRequestID
}

// errorHolder is a mutable container stored in context so handlers can set
// an error reason after the middleware has already captured the context.
type errorHolder struct{ msg string }

// WithErrorSlot stores an empty error holder in ctx.
// Must be called by middleware before ServeHTTP so handlers can write to it.
func WithErrorSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, errorKey{}, &errorHolder{})
}

// SetError writes a short error reason into the context slot.
// No-op if the slot was not initialized.
func SetError(ctx context.Context, msg string) {
	if h, ok := ctx.Value(errorKey{}).(*errorHolder); ok {
		h.msg = msg
	}
}

// TryError returns the error reason from the context (empty if none).
func TryError(ctx context.Context) string {
	if h, ok := ctx.Value(errorKey{}).(*errorHolder); ok {
		return h.msg
	}
	return ""
}
