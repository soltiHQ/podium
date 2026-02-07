package interceptor

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// UnaryLogger returns a unary server interceptor that logs every completed call.
func UnaryLogger(logger zerolog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()

		resp, err := handler(ctx, req)

		st, _ := grpcstatus.FromError(err)
		code := st.Code()

		evt := logger.Info()
		if err != nil {
			evt = logger.Error().Err(err)
		}

		evt.
			Str("method", info.FullMethod).
			Str("code", code.String()).
			Dur("duration", time.Since(start))

		if rid, ok := transportctx.RequestID(ctx); ok {
			evt = evt.Str("request_id", rid)
		}

		evt.Msg("grpc request")

		return resp, err
	}
}
