package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownConcurrentCallersAndTimedOutWaiter(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	a := &App{shutdownDone: make(chan struct{}), raftShutdown: func() { calls.Add(1); <-release }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.Shutdown(ctx); err != context.DeadlineExceeded {
		t.Fatalf("want bounded waiting: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Go(func() {
			if err := a.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("cleanup ran %d times", calls.Load())
	}
}
