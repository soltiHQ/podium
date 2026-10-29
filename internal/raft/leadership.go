package raft

import (
	"context"
	"sync"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

var _ cluster.Leadership = (*Leadership)(nil)

// Leadership is backed by the Raft node's authoritative state. Each
// WhenLeader caller observes transitions independently: LeaderCh is a single
// channel whose messages would otherwise be divided between background runners.
type Leadership struct{ r *hraft.Raft }

func NewLeadership(r *hraft.Raft) *Leadership {
	if r == nil {
		panic("raft: nil raft")
	}
	return &Leadership{r: r}
}

// WaitReady waits for all preceding log operations to reach the FSM. Election
// alone does not imply restored state is ready for bootstrap or reconciliation.
func (l *Leadership) WaitReady(ctx context.Context) error { return awaitApplied(ctx, l.r) }
func (l *Leadership) AmLeader() bool                      { return l.r.State() == hraft.Leader }
func (l *Leadership) CurrentLeader() string               { addr, _ := l.r.LeaderWithID(); return string(addr) }

func (l *Leadership) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil || ctx.Err() != nil {
		return nil
	}
	type lease struct{ cancel context.CancelFunc }
	var mu sync.Mutex
	var active *lease
	wake := make(chan struct{}, 1)
	observer := hraft.NewObserver(nil, false, func(observation *hraft.Observation) bool {
		state, ok := observation.Data.(hraft.RaftState)
		if !ok {
			return false
		}
		if state != hraft.Leader {
			mu.Lock()
			current := active
			mu.Unlock()
			// Cancellation follows every actual loss transition synchronously. Wake
			// signals may coalesce, but they never carry the active lease's cancellation.
			if current != nil {
				current.cancel()
			}
		}
		select {
		case wake <- struct{}{}:
		default:
		}
		return false
	})
	// Register before sampling State so a transition cannot slip between startup
	// and subscription. The observer owns no goroutine and never blocks on a queue.
	l.r.RegisterObserver(observer)
	defer l.r.DeregisterObserver(observer)
	finish := func(current *lease) {
		mu.Lock()
		if active == current {
			active = nil
		}
		mu.Unlock()
		current.cancel()
	}
	defer func() {
		mu.Lock()
		current := active
		active = nil
		mu.Unlock()
		if current != nil {
			current.cancel()
		}
	}()
	for {
		for !l.AmLeader() {
			if ctx.Err() != nil || l.r.State() == hraft.Shutdown {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		leaderCtx, cancel := context.WithCancel(ctx)
		current := &lease{cancel: cancel}
		mu.Lock()
		active = current
		stillLeader := l.AmLeader()
		mu.Unlock()
		if !stillLeader {
			finish(current)
			continue
		}
		if err := l.WaitReady(leaderCtx); err != nil {
			lost := leaderCtx.Err() != nil || !l.AmLeader()
			finish(current)
			if ctx.Err() != nil {
				return nil
			}
			if lost {
				continue
			}
			return err
		}
		if leaderCtx.Err() != nil {
			finish(current)
			continue
		}
		err := fn(leaderCtx)
		finish(current)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}
