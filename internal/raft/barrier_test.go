package raft

import (
	"context"
	"errors"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

func TestAwaitOperationBoundsEntireFuture(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := awaitOperation(ctx, func() error { <-blocked; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unapplied state accepted: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("caller waited for the stalled Raft future")
	}
}

func TestAwaitOperationPropagatesBarrierFailure(t *testing.T) {
	want := errors.New("lost leadership")
	if err := awaitOperation(context.Background(), func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("barrier failure lost: %v", err)
	}
}

// A barrier writes nothing, so one that fails with ErrLeadershipLost left no write
// in doubt: the transaction that waited for it did not run. It fails with
// ErrNotLeader, which a client may retry, and not with cluster.ErrLeadershipLost,
// which says the write may still commit; hashicorp's own error stays found by
// errors.Is. The other errors of a barrier mean what clusterError says.
func TestAwaitBarrierLeadershipLostIsNotLeader(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		meaning error // what errors.Is must find
		not     error // what it must not find
	}{
		{"leadership lost", hraft.ErrLeadershipLost, cluster.ErrNotLeader, cluster.ErrLeadershipLost},
		{"not the leader", hraft.ErrNotLeader, cluster.ErrNotLeader, cluster.ErrLeadershipLost},
		{"shutdown", hraft.ErrRaftShutdown, storage.ErrUnavailable, cluster.ErrNotLeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := awaitBarrier(context.Background(), func() error { return tc.err })
			if !errors.Is(err, tc.meaning) || errors.Is(err, tc.not) || !errors.Is(err, tc.err) {
				t.Fatalf("awaitBarrier = %v, want %v and %v, not %v", err, tc.meaning, tc.err, tc.not)
			}
			if kind := errkind.Classify(err); kind != errkind.Unavailable {
				t.Fatalf("errkind.Classify(%v) = %v, want Unavailable", err, kind)
			}
		})
	}
}
