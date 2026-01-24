// Package transportctx provides helpers for storing transport-scoped contextual values.
package transportctx

import "context"

type key int

const (
	requestIDKey key = iota
	identityKey
)

// WithRequestID returns a context carrying the given request ID.
func WithRequestID(parent context.Context, id string) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	if id == "" {
		return parent
	}
	return context.WithValue(parent, requestIDKey, id)
}

// RequestID returns the request ID stored in the context.
// The second return value reports whether the ID was set.
func RequestID(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(requestIDKey).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// WithIdentity stores the authenticated identity in the context.
func WithIdentity(parent context.Context, identity *auth.Identity) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	if identity == nil {
		return parent
	}
	return context.WithValue(parent, identityKey, identity)
}

// Identity returns the authenticated identity stored in the context.
// The second return value reports whether the identity was set.
func Identity(ctx context.Context) (*auth.Identity, bool) {
	if ctx == nil {
		return nil, false
	}
	id, ok := ctx.Value(identityKey).(*auth.Identity)
	if !ok || id == nil {
		return nil, false
	}
	return id, true
}
