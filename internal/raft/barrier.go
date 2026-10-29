package raft

import (
	"context"
	"fmt"

	hraft "github.com/hashicorp/raft"
)

// Barrier's timeout only bounds enqueueing, not Future.Error. Bound the entire
// readiness operation and let Raft finish its future after caller cancellation.
func awaitApplied(ctx context.Context, r *hraft.Raft) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := awaitOperation(readyCtx, func() error { return r.Barrier(applyTimeout).Error() }); err != nil {
		return fmt.Errorf("raft: wait for applied state: %w", err)
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
