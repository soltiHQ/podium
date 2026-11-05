package httpctx

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
)

type (
	responderKey struct{}
	leaderKey    struct{}
)

// fallback is an immutable sentinel returned when no responder was negotiated.
var fallback responder.Responder = responder.NewJSON()

// WithResponder stores the negotiated responder in ctx.
func WithResponder(ctx context.Context, r responder.Responder) context.Context {
	return context.WithValue(ctx, responderKey{}, r)
}

// Responder returns the negotiated responder from ctx.
func Responder(ctx context.Context) responder.Responder {
	if r, ok := ctx.Value(responderKey{}).(responder.Responder); ok && r != nil {
		return r
	}
	return fallback
}

// WithLeader stores in ctx the function that tells where the cluster leader
// serves the listener the request came in on. It is a function, not an address,
// because the leader can change while the request runs.
func WithLeader(ctx context.Context, addr func() string) context.Context {
	return context.WithValue(ctx, leaderKey{}, addr)
}

// Leader returns the HTTP address (host:port) at which the cluster leader
// serves the listener of the request. It is empty when the request does not
// come through the Leader middleware, when this node is the leader, and when no
// leader is known: no hint then reaches the client.
func Leader(ctx context.Context) string {
	if addr, ok := ctx.Value(leaderKey{}).(func() string); ok && addr != nil {
		return addr()
	}
	return ""
}
