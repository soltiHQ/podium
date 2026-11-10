package metrics

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/felixge/httpsnoop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// routeOther is the route of a request that matched no mux pattern.
const routeOther = "other"

// routeKey is the context key of a routeSlot.
type routeKey struct{}

// routeSlot carries the matched mux pattern from [Route], which sees it, to the
// middleware of [Metrics.HTTP], which reports it.
type routeSlot struct{ pattern string }

// Route wraps mux, the innermost handler of a listener, so that the middleware of
// [Metrics.HTTP], outside it, can name the route a request matched. The mux sets
// the Pattern of the request it serves, and the middleware in between replace the
// request with copies, so only a wrapper of the mux itself can read it; Route
// hands it to the slot that Metrics.HTTP put in the context.
func Route(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if slot, ok := r.Context().Value(routeKey{}).(*routeSlot); ok {
			slot.pattern = r.Pattern
		}
	})
}

// HTTP returns the middleware that counts and times the requests of the listener
// server, "ui" or "discovery". Put it outside every other middleware, with [Route]
// around the mux, so that a request that an inner middleware answers is counted
// too. The route label is the matched mux pattern, or "other"; the method label is
// one of the standard methods, or "other". A response that is an event stream is
// counted and not timed.
func (m *Metrics) HTTP(server string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			slot := &routeSlot{}
			r = r.WithContext(context.WithValue(r.Context(), routeKey{}, slot))
			captured := httpsnoop.CaptureMetrics(next, w, r)

			route, method := slot.pattern, requestMethod(r.Method)
			if route == "" {
				route = routeOther
			}
			m.httpRequests.WithLabelValues(server, route, method, strconv.Itoa(captured.Code)).Inc()
			if w.Header().Get("Content-Type") != "text/event-stream" {
				m.httpDuration.WithLabelValues(server, route, method).Observe(time.Since(start).Seconds())
			}
		})
	}
}

// requestMethod is method when it is one of the standard ones, so that a client
// cannot make up a label value, and "other" otherwise.
func requestMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	}
	return routeOther
}

// UnaryInterceptor returns the unary server interceptor that counts and times
// the calls by full method and status code. Put it first in the chain, so that a
// call that an inner interceptor refuses is counted too.
func (m *Metrics) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if m == nil {
			return handler(ctx, req)
		}
		start := time.Now()
		resp, err := handler(ctx, req)
		m.grpcRequests.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
		m.grpcDuration.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

// StreamInterceptor returns the stream server interceptor that counts the streams
// by full method and status code, once each ends. It does not time them: a stream
// lasts as long as its client stays.
func (m *Metrics) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		err := handler(srv, ss)
		if m != nil {
			m.grpcRequests.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
		}
		return err
	}
}
