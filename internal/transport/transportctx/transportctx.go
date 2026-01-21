// Package transportctx provides helpers for storing transport-scoped contextual values.
package transportctx

import "context"

type key int

const requestIDKey key = iota

// WithRequestID returns a context carrying the given request ID.
func WithRequestID(parent context.Context, id string) context.Context {
	return context.WithValue(parent, requestIDKey, id)
}

// RequestID returns the request ID stored in the context.
// The second return value reports whether the ID was set.
func RequestID(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(requestIDKey).(string)
	if !ok {
		return "", false
	}
	return v, true
}
