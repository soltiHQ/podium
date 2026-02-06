package response

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/transportctx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const headerRequestID = "x-request-id"

// SetRequestID sets x-request-id header on the outgoing gRPC response if present in ctx.
// Safe to call multiple times; best-effort (ignores errors).
func SetRequestID(ctx context.Context) {
	if rid, ok := transportctx.RequestID(ctx); ok && rid != "" {
		_ = grpc.SetHeader(ctx, metadata.Pairs(headerRequestID, rid))
	}
}
