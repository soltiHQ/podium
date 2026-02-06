package interceptors

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/transportctx"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const requestIDKey = "x-request-id"

// UnaryRequestID ensures request id exists in context for unary calls.
func UnaryRequestID() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		rid := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get(requestIDKey); len(vals) != 0 {
				rid = transportctx.NormalizeRequestID(vals[0])
			}
		}
		if rid == "" {
			rid = transportctx.NewRequestID()
		}
		return handler(transportctx.WithRequestID(ctx, rid), req)
	}
}
