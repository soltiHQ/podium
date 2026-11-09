package status

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func TestMapError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"context canceled", context.Canceled, codes.Canceled},
		{"deadline exceeded", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"invalid token → unauthenticated", auth.ErrInvalidToken, codes.Unauthenticated},
		{"revoked → unauthenticated", auth.ErrRevoked, codes.Unauthenticated},
		{"unauthorized → permission denied", auth.ErrUnauthorized, codes.PermissionDenied},
		{"auth invalid argument", auth.ErrInvalidArgument, codes.InvalidArgument},
		{"user disabled → failed precondition", auth.ErrUserDisabled, codes.FailedPrecondition},
		{"not found", storage.ErrNotFound, codes.NotFound},
		{"already exists", storage.ErrAlreadyExists, codes.AlreadyExists},
		{"conflict → aborted", storage.ErrConflict, codes.Aborted},
		{"storage invalid argument", storage.ErrInvalidArgument, codes.InvalidArgument},
		{"not the leader → unavailable", fmt.Errorf("raft: apply: %w", cluster.ErrNotLeader), codes.Unavailable},
		{"unknown → internal", errors.New("boom"), codes.Internal},
		{"wrapped sentinel is unwrapped", fmt.Errorf("load: %w", storage.ErrNotFound), codes.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, msg := mapError(tt.err)
			if got != tt.want {
				t.Fatalf("mapError(%v) code = %v, want %v", tt.err, got, tt.want)
			}
			if msg == "" {
				t.Fatalf("mapError(%v) returned empty message", tt.err)
			}
		})
	}
}

func TestFromErrorNil(t *testing.T) {
	st := FromError(context.Background(), nil)
	if st.Code() != codes.OK {
		t.Fatalf("FromError(nil) code = %v, want OK", st.Code())
	}
}

// Throttled answers a call that a rate limit refused: ResourceExhausted with the
// message, RetryInfo with the delay after which the client may try again, the
// gRPC form of Retry-After, which is also how UnaryLogger tells a throttle from
// any other ResourceExhausted, and the request ID, as every error carries it.
func TestThrottled(t *testing.T) {
	ctx := transportctx.WithErrorSlot(transportctx.WithRequestID(context.Background(), "rid-1"))
	err := Throttled(ctx, 1500*time.Millisecond, "slow down: %d", 7)

	st := grpcstatus.Convert(err)
	if st.Code() != codes.ResourceExhausted || st.Message() != "slow down: 7" {
		t.Fatalf("Throttled = %v, want ResourceExhausted: slow down: 7", err)
	}
	var (
		retry   *errdetails.RetryInfo
		request *errdetails.RequestInfo
	)
	for _, detail := range st.Details() {
		switch d := detail.(type) {
		case *errdetails.RetryInfo:
			retry = d
		case *errdetails.RequestInfo:
			request = d
		}
	}
	if retry == nil || retry.GetRetryDelay().AsDuration() != 1500*time.Millisecond {
		t.Fatalf("RetryInfo = %v, want a delay of 1.5s", retry)
	}
	if request == nil || request.GetRequestId() != "rid-1" {
		t.Fatalf("RequestInfo = %v, want the request ID rid-1", request)
	}
	if got := transportctx.TryError(ctx); got != "slow down: 7" {
		t.Fatalf("error slot = %q, want the message", got)
	}
}

// An error that is not a throttle carries no RetryInfo: UnaryLogger would take
// it for one.
func TestErrorfCarriesNoRetryInfo(t *testing.T) {
	st := grpcstatus.Convert(Errorf(context.Background(), codes.ResourceExhausted, "rate limited"))
	for _, detail := range st.Details() {
		if _, ok := detail.(*errdetails.RetryInfo); ok {
			t.Fatal("Errorf attached RetryInfo")
		}
	}
}
