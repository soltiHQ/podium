package response

import (
	"context"
)

type responderKey struct{}

// WithResponder stores a Responder in ctx.
func WithResponder(ctx context.Context, r Responder) context.Context {
	return context.WithValue(ctx, responderKey{}, r)
}

// FromContext extracts Responder from ctx.
// Returns a default JSONResponder if none was set.
func FromContext(ctx context.Context) Responder {
	if r, ok := ctx.Value(responderKey{}).(Responder); ok {
		return r
	}
	return fallbackJSON
}

var fallbackJSON Responder = NewJSON()
