package auth

import (
	"context"

	authcore "github.com/soltiHQ/control-plane/auth"
	"github.com/soltiHQ/control-plane/internal/transportctx"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// Unary server interceptor.
func Unary(verifier authcore.Verifier, logger zerolog.Logger) grpc.UnaryServerInterceptor {
	l := logger.With().Str("middleware", "auth_grpc").Logger()

	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		var (
			rawAuth string
			ip      string
		)
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("authorization"); len(vals) > 0 {
				rawAuth = vals[0]
			}
		}
		if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
			ip = p.Addr.String()
		}
		token := extractBearerToken(rawAuth)
		if token == "" {
			ev := l.Warn().
				Str("method", info.FullMethod).
				Str("remote_addr", ip).
				Str("reason", "missing or malformed authorization metadata")

			if reqID, ok := transportctx.RequestID(ctx); ok {
				ev = ev.Str("request_id", reqID)
			}
			ev.Msg("auth: unauthorized gRPC request")

			return nil, grpcErrorFromAuth(authcore.ErrInvalidToken)
		}

		id, err := verifier.Verify(ctx, token)
		if err != nil {
			ev := l.Warn().
				Err(err).
				Str("method", info.FullMethod).
				Str("remote_addr", ip)

			if reqID, ok := transportctx.RequestID(ctx); ok {
				ev = ev.Str("request_id", reqID)
			}
			if id != nil && id.Subject != "" {
				ev = ev.Str("subject", id.Subject)
			}
			ev.Msg("auth: gRPC verification failed")

			return nil, grpcErrorFromAuth(err)
		}
		ctx = transportctx.WithIdentity(ctx, id)
		return handler(ctx, req)
	}
}
