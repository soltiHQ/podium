package httpctx

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
)

type responderKey struct{}

var fallback responder.Responder = responder.NewJSON()

func WithResponder(ctx context.Context, r responder.Responder) context.Context {
	return context.WithValue(ctx, responderKey{}, r)
}

// Responder returns a responder from context.
func Responder(ctx context.Context) responder.Responder {
	if r, ok := ctx.Value(responderKey{}).(responder.Responder); ok && r != nil {
		return r
	}
	return fallback
}
