package raft

import (
	"context"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

var _ cluster.Leadership = (*Leadership)(nil)

// Leadership is [cluster.Leadership] backed by the raft node state.
//
// WhenLeader uses raft.LeaderCh() — a bool channel that receives true when
// we gain leadership and false when we lose it. Between signals state may
// fluctuate briefly; AmLeader always reads the authoritative raft.State().
type Leadership struct {
	r *hraft.Raft
}

func NewLeadership(r *hraft.Raft) *Leadership {
	if r == nil {
		panic("raft: nil raft")
	}
	return &Leadership{r: r}
}

func (l *Leadership) AmLeader() bool { return l.r.State() == hraft.Leader }

func (l *Leadership) CurrentLeader() string {
	addr, _ := l.r.LeaderWithID()
	return string(addr)
}

func (l *Leadership) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil {
		return nil
	}
	ch := l.r.LeaderCh()

	for {
		// Wait for leadership.
		for !l.AmLeader() {
			select {
			case <-ctx.Done():
				return nil
			case _, ok := <-ch:
				if !ok {
					return nil
				}
			}
		}

		leaderCtx, cancel := context.WithCancel(ctx)
		// Watcher cancels leaderCtx on any LeaderCh signal that lands us
		// out of leadership.
		go func() {
			for {
				select {
				case <-leaderCtx.Done():
					return
				case _, ok := <-ch:
					if !ok || !l.AmLeader() {
						cancel()
						return
					}
				}
			}
		}()

		if err := fn(leaderCtx); err != nil {
			cancel()
			return err
		}
		cancel()
		if ctx.Err() != nil {
			return nil
		}
	}
}
