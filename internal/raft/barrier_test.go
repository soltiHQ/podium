package raft

import (
	"context"
	"errors"
	"testing"
	"time"
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
