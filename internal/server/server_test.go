package server

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type stuckRunner struct {
	started chan struct{}
	release chan struct{}
	stops   atomic.Int32
}

func (*stuckRunner) Name() string                  { return "stuck" }
func (r *stuckRunner) Start(context.Context) error { close(r.started); <-r.release; return nil }
func (r *stuckRunner) Stop(context.Context) error  { r.stops.Add(1); <-r.release; return nil }
func TestRunBoundsUncooperativeRunnerShutdown(t *testing.T) {
	r := &stuckRunner{started: make(chan struct{}), release: make(chan struct{})}
	s, _ := New(Config{ShutdownTimeout: 25 * time.Millisecond}, zerolog.New(io.Discard), r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-r.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("missing drain timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run waited forever after shutdown deadline")
	}
	close(r.release)
	_ = s.Shutdown(context.Background())
	if r.stops.Load() != 1 {
		t.Fatal("runner stopped twice")
	}
}
