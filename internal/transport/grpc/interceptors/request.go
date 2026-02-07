package interceptor

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

const metadataKeyRequestID = "x-request-id"

// UnaryRequestID returns a unary server interceptor that ensures every request has a unique ID.
func UnaryRequestID() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = ensureRequestID(ctx)
		return handler(ctx, req)
	}
}

func ensureRequestID(ctx context.Context) context.Context {
	rid := extractRequestID(ctx)
	if rid == "" {
		rid = transportctx.NewRequestID()
	}

	ctx = transportctx.WithRequestID(ctx, rid)

	// Echo back in response headers so the client can correlate.
	_ = grpc.SetHeader(ctx, metadata.Pairs(metadataKeyRequestID, rid))

	return ctx
}

func extractRequestID(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(metadataKeyRequestID)
	if len(vals) == 0 {
		return ""
	}
	return transportctx.NormalizeRequestID(vals[0])
}
