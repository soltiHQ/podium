package raft

import (
	"context"
	"fmt"

	hraft "github.com/hashicorp/raft"
)

// Barrier's timeout only bounds enqueueing, not Future.Error. Bound the entire
// readiness operation and let Raft finish its future after caller cancellation.
// A barrier writes nothing, so its failure never leaves a write in doubt (see
// barrierError); a node that is not the leader fails with ErrNotLeader.
func awaitApplied(ctx context.Context, r *hraft.Raft) error {
	return awaitBarrier(ctx, func() error { return r.Barrier(applyTimeout).Error() })
}

// awaitBarrier is awaitApplied over the barrier operation it waits for.
func awaitBarrier(ctx context.Context, barrier func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := awaitOperation(readyCtx, barrier); err != nil {
		return fmt.Errorf("raft: wait for applied state: %w", barrierError(err))
	}
	return nil
}

func awaitOperation(ctx context.Context, operation func() error) error {
	completed := make(chan error, 1)
	go func() { completed <- operation() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-completed:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}
