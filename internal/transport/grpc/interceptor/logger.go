package interceptor

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// UnaryLogger returns a unary server interceptor that logs every completed call: at INFO, and at ERROR when it
// fails. The exception is an Unavailable that carries the x-leader hint, which is logged at WARN: a write that
// reached a follower is a routing condition and not a failure of this node, as the HTTP access log has it for a 503
// with X-Leader, and only the interceptors that route writes set the hint (setLeaderHint), inside this one. Any
// other failure stays an ERROR, an Unavailable without the hint included.
func UnaryLogger(logger zerolog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		ctx, hint := withHintSlot(ctx)

		var (
			resp, err = handler(ctx, req)
			st, _     = grpcstatus.FromError(err)

			code = st.Code()
			evt  = logger.Info()
		)
		switch {
		case code == codes.Unavailable && hint.set.Load():
			evt = logger.Warn().Err(err)
		case err != nil:
			evt = logger.Error().Err(err)
		}

		evt.
			Str("method", info.FullMethod).
			Str("code", code.String()).
			Str("duration", time.Since(start).String())

		if rid, ok := transportctx.RequestID(ctx); ok {
			evt = evt.Str("request_id", rid)
		}
		if reason := transportctx.TryError(ctx); reason != "" {
			evt = evt.Str("error", reason)
		}

		evt.Msg("grpc request")
		return resp, err
	}
}
