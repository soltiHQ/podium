package loghub_test

import (
	"context"
	"errors"
	"testing"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/internal/loghub"
)

func TestBlockedOpenDoesNotBlockOtherTasksAndCloseCancels(t *testing.T) {
	blocked := make(chan struct{})
	cancelled := make(chan struct{})
	hub := loghub.New(func(ctx context.Context, _, task string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		if task == "blocked" {
			close(blocked)
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		}
		return make(chan *taskv1.StreamTaskLogsResponse), nil
	}, loghub.Options{})
	done := make(chan error, 1)
	go func() { _, _, err := hub.Subscribe(context.Background(), "agent", "blocked"); done <- err }()
	<-blocked
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, unsub, err := hub.Subscribe(ctx, "agent", "healthy")
	if err != nil {
		t.Fatalf("unrelated source blocked behind open: %v", err)
	}
	unsub()
	hub.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("opening source not cancelled")
	}
	if err := <-done; err == nil {
		t.Fatal("closed opening subscription succeeded")
	}
	if _, _, err := hub.Subscribe(context.Background(), "agent", "later"); !errors.Is(err, loghub.ErrSourceClosed) {
		t.Fatal("closed hub reopened")
	}
}

func TestOpenTimeoutAndCallerCancellation(t *testing.T) {
	hub := loghub.New(func(ctx context.Context, _, _ string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, loghub.Options{OpenTimeout: 20 * time.Millisecond})
	defer hub.Close()
	start := time.Now()
	_, _, err := hub.Subscribe(context.Background(), "agent", "task")
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("unbounded opening: %v", err)
	}
}

func TestCloseStopsProducerThatNeverClosesChannel(t *testing.T) {
	hub := loghub.New(func(ctx context.Context, _, _ string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		return make(chan *taskv1.StreamTaskLogsResponse), nil
	}, loghub.Options{})
	ch, _, err := hub.Subscribe(context.Background(), "agent", "task")
	if err != nil {
		t.Fatal(err)
	}
	hub.Close()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed subscription")
		}
	case <-time.After(time.Second):
		t.Fatal("source pump ignores cancellation")
	}
}
