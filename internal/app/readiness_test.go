package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recoveringLeader struct {
	leader bool
	ready  chan struct{}
	called chan struct{}
}

func (l *recoveringLeader) AmLeader() bool        { return l.leader }
func (l *recoveringLeader) CurrentLeader() string { return "elected" }
func (l *recoveringLeader) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func (l *recoveringLeader) WaitReady(ctx context.Context) error {
	close(l.called)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ready:
		return nil
	}
}

func TestBootstrapWaitsForRestoredLeaderState(t *testing.T) {
	leader := &recoveringLeader{leader: true, ready: make(chan struct{}), called: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		seed, err := readyForBootstrap(context.Background(), leader, time.Second)
		if err == nil && !seed {
			err = errors.New("leader seeding skipped")
		}
		result <- err
	}()
	<-leader.called
	select {
	case err := <-result:
		t.Fatalf("bootstrap started before replay: %v", err)
	default:
	}
	close(leader.ready)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapReplayFailurePreventsSeeding(t *testing.T) {
	leader := &recoveringLeader{leader: true, ready: make(chan struct{}), called: make(chan struct{})}
	seed, err := readyForBootstrap(context.Background(), leader, 20*time.Millisecond)
	if seed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("seed=%v, err=%v", seed, err)
	}
}

func TestFollowerSkipsLocalReplayAndBootstrap(t *testing.T) {
	leader := &recoveringLeader{ready: make(chan struct{}), called: make(chan struct{})}
	seed, err := readyForBootstrap(context.Background(), leader, time.Second)
	if seed || err != nil {
		t.Fatalf("seed=%v, err=%v", seed, err)
	}
	select {
	case <-leader.called:
		t.Fatal("follower attempted leader replay barrier")
	default:
	}
}
