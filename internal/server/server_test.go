package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
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

// stoppedFirst is a listener runner whose Start waits until its Stop has
// returned: the order in which Run stops a runner whose goroutine has not
// reached Start yet, forced.
type stoppedFirst struct {
	*httpserver.Runner
	stopped chan struct{}
}

func (r *stoppedFirst) Start(ctx context.Context) error {
	<-r.stopped
	return r.Runner.Start(ctx)
}

func (r *stoppedFirst) Stop(ctx context.Context) error {
	defer close(r.stopped)
	return r.Runner.Stop(ctx)
}

// A runner that fails to bind ends Run at once, though another listener runner
// is stopped before its Start runs: that one never binds, so Run does not wait
// out the shutdown timeout for it, and reports the runner that failed.
func TestRunReturnsAtOnceWhenARunnerIsStoppedBeforeItStarts(t *testing.T) {
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	if err = free.Close(); err != nil {
		t.Fatal(err)
	}
	late, err := httpserver.New(httpserver.Config{Name: "late", Addr: addr}, zerolog.Nop(), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	failing, err := httpserver.New(httpserver.Config{Name: "taken", Addr: taken.Addr().String()}, zerolog.Nop(), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{ShutdownTimeout: time.Minute}, zerolog.New(io.Discard), &stoppedFirst{Runner: late, stopped: make(chan struct{})}, failing)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of a runner that could not bind")
	}
	var failed *RunnerError
	if !errors.As(err, &failed) || failed.Runner != "taken" || failed.Phase != "start" {
		t.Fatalf("Run: %v, want the start error of the runner that could not bind", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v, want no shutdown timeout", err)
	}
	if got := late.Addr(); got != "" {
		t.Fatalf("the runner stopped before it started is bound at %s", got)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the address of the runner stopped before it started is taken after Run: %v", err)
	}
	_ = l.Close()
}

// endsWithItsContext is a runner whose Start returns nil once its context ends,
// as the tick runners' does: it ends with the shutdown signal, not on its own.
type endsWithItsContext struct{ name string }

func (r endsWithItsContext) Name() string                  { return r.name }
func (endsWithItsContext) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (endsWithItsContext) Stop(context.Context) error      { return nil }

// A runner that returns because the shutdown signal cancelled its context did
// not exit on its own: Run reports the signal, exactly context.Canceled, though
// that runner's return is ready as soon as the signal is. Many runners and many
// runs make it all but certain that a return is ready when Run looks.
func TestRunReportsTheSignalWhenRunnersEndWithIt(t *testing.T) {
	runners := make([]Runner, 0, 32)
	for i := range 32 {
		runners = append(runners, endsWithItsContext{name: fmt.Sprintf("tick-%d", i)})
	}
	for range 200 {
		s, err := New(Config{}, zerolog.New(io.Discard), runners...)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.Run(ctx); err != context.Canceled {
			t.Fatalf("Run: %v, want exactly context.Canceled", err)
		}
	}
}
