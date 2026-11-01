package base_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
	"github.com/soltiHQ/control-plane/internal/server/runner/base"
)

func discardLogger() zerolog.Logger { return zerolog.New(io.Discard) }

func TestTicksWhileLeaderAndStopDrains(t *testing.T) {
	var ticks atomic.Int64
	r := base.New("test", time.Millisecond, standalone.NewLeadership(), discardLogger(),
		func(context.Context) { ticks.Add(1) })

	errCh := make(chan error, 1)
	go func() { errCh <- r.Start(context.Background()) }()

	deadline := time.After(2 * time.Second)
	for ticks.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no tick observed while leader")
		case <-time.After(time.Millisecond):
		}
	}

	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after Stop drained")
	}
}

func TestStopHonorsDeadlineWhenTickStuck(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	r := base.New("test", time.Millisecond, standalone.NewLeadership(), discardLogger(),
		func(context.Context) {
			startOnce.Do(func() { close(started) })
			<-release
		})

	errCh := make(chan error, 1)
	go func() { errCh <- r.Start(context.Background()) }()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop: want DeadlineExceeded, got %v", err)
	}

	close(release)
	select {
	case err := <-errCh:
		_ = err // Start only has to return; its result is not under test.
	case <-time.After(time.Second):
		t.Fatal("Start did not return after release")
	}
}

func TestStopIdempotent(t *testing.T) {
	r := base.New("test", time.Millisecond, standalone.NewLeadership(), discardLogger(),
		func(context.Context) {})

	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}

	go func() { _ = r.Start(context.Background()) }()
	time.Sleep(10 * time.Millisecond)

	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStartTwice(t *testing.T) {
	r := base.New("test", time.Second, standalone.NewLeadership(), discardLogger(),
		func(context.Context) {})

	go func() { _ = r.Start(context.Background()) }()
	time.Sleep(10 * time.Millisecond)

	if err := r.Start(context.Background()); !errors.Is(err, base.ErrAlreadyStarted) {
		t.Fatalf("second Start: want ErrAlreadyStarted, got %v", err)
	}
	_ = r.Stop(context.Background())
}

// terms is a Leadership that leads for a fixed number of terms, each ended by end.
type terms struct {
	n      int
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (*terms) AmLeader() bool        { return true }
func (*terms) CurrentLeader() string { return "" }

func (l *terms) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	for range l.n {
		term, cancel := context.WithCancel(ctx)
		l.mu.Lock()
		l.cancel = cancel
		l.mu.Unlock()
		err := fn(term)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *terms) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancel()
}

func TestOnLeaderRunsBeforeTheFirstTickOfEachTerm(t *testing.T) {
	leadership := &terms{n: 3}
	var events []string
	r := base.New("test", time.Millisecond, leadership, discardLogger(), func(context.Context) {
		events = append(events, "tick")
		leadership.end()
	})
	r.OnLeader(func() { events = append(events, "leader") })

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the last term")
	}

	// Each term opens with the hook and ticks at least once after it.
	if len(events) < 6 || events[0] != "leader" {
		t.Fatalf("events %v", events)
	}
	hooks := 0
	for i, event := range events {
		if event == "leader" {
			hooks++
			if i+1 == len(events) || events[i+1] != "tick" {
				t.Fatalf("a term ended without a tick: %v", events)
			}
		}
	}
	if hooks != 3 {
		t.Fatalf("hook ran %d times in 3 terms: %v", hooks, events)
	}
}
