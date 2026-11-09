package interceptor

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// UnaryLogger returns a unary server interceptor that logs every completed call: at INFO, and at ERROR when it
// fails. A grpc.health.v1 call is logged at DEBUG whatever its answer (isHealthProbe): supervisors and load balancers
// probe every few seconds, and a probe that names an unknown service would write an ERROR each time. There are two
// other exceptions. The Unavailable "not the leader" answer with the x-leader hint is logged at WARN: a
// write that reached a follower is a routing condition and not a failure of this node, as the HTTP access log has
// it for a 503 with X-Leader, and only the interceptors that route writes, inside this one, mark it
// (hintNotLeader). Any other failure stays an ERROR: an Unavailable without the hint, and "leader unreachable",
// which keeps its hint, as the HTTP access log keeps the 502 of an unreachable leader an ERROR. A ResourceExhausted
// that carries RetryInfo is logged at DEBUG: it is a throttle, a client that sent more than its share and was told
// when to come back, which any client can cause at will, as the HTTP access log has it for a 429 with Retry-After,
// and only status.Throttled attaches that detail. Any other ResourceExhausted stays an ERROR. A follower that
// forwards a call passes the leader's error on with its details, and logs the same line for it.
func UnaryLogger(logger zerolog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		ctx, slot := withHintSlot(ctx)

		var (
			resp, err = handler(ctx, req)
			st, _     = grpcstatus.FromError(err)

			code = st.Code()
			evt  = logger.Info()
		)
		switch {
		case isHealthProbe(info.FullMethod):
			evt = logger.Debug()
			if err != nil {
				evt = evt.Err(err)
			}
		case code == codes.Unavailable && slot.notLeader.Load():
			evt = logger.Warn().Err(err)
		case throttle(st):
			evt = logger.Debug().Err(err)
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

// healthService is the prefix of the full methods of grpc.health.v1: Check and
// Watch.
const healthService = "/grpc.health.v1.Health/"

// isHealthProbe reports whether fullMethod is a method of the health service,
// which the access log keeps at DEBUG whatever the call's answer.
func isHealthProbe(fullMethod string) bool { return strings.HasPrefix(fullMethod, healthService) }

// throttle reports whether st is a throttle: a ResourceExhausted with RetryInfo.
func throttle(st *grpcstatus.Status) bool {
	if st.Code() != codes.ResourceExhausted {
		return false
	}
	for _, detail := range st.Details() {
		if _, ok := detail.(*errdetails.RetryInfo); ok {
			return true
		}
	}
	return false
}
