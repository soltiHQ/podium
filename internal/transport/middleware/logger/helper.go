package logger

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/soltiHQ/control-plane/internal/transportctx"
	
	"github.com/rs/zerolog"
	"google.golang.org/grpc/peer"
)

func withRequestID(ctx context.Context, ev *zerolog.Event) *zerolog.Event {
	if reqID, ok := transportctx.RequestID(ctx); ok {
		ev = ev.Str("request_id", reqID)
	}
	return ev
}

func remoteAddrHTTP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}

func remoteAddrGRPC(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
			return host
		}
		return p.Addr.String()
	}
	return ""
}
