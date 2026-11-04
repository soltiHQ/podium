package raft

import (
	"context"
	"sync"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

var (
	_ cluster.Leadership = (*Leadership)(nil)
	_ cluster.Membership = (*Leadership)(nil)
)

// readyRetryPause is how long WhenLeader waits, after a WaitReady that failed
// while this node still leads, before it tries again.
const readyRetryPause = time.Second

// Leadership is backed by the Raft node's authoritative state. Each
// WhenLeader caller observes transitions independently: LeaderCh is a single
// channel whose messages would otherwise be divided between background runners.
type Leadership struct {
	r *hraft.Raft
	// logger gets the warnings of WhenLeader.
	logger zerolog.Logger
	// ready is the wait of WaitReady, the barrier of awaitApplied; tests replace it.
	ready func(context.Context) error
	// retryPause is readyRetryPause; tests shorten it.
	retryPause time.Duration
}

// NewLeadership returns the leadership of r, which logs its warnings into logger.
func NewLeadership(r *hraft.Raft, logger zerolog.Logger) *Leadership {
	if r == nil {
		panic("raft: nil raft")
	}
	return &Leadership{
		r:          r,
		logger:     logger,
		ready:      func(ctx context.Context) error { return awaitApplied(ctx, r) },
		retryPause: readyRetryPause,
	}
}

// WaitReady waits for all preceding log operations to reach the FSM. Election
// alone does not imply restored state is ready for bootstrap or reconciliation.
func (l *Leadership) WaitReady(ctx context.Context) error { return l.ready(ctx) }
func (l *Leadership) AmLeader() bool                      { return l.r.State() == hraft.Leader }
func (l *Leadership) CurrentLeader() string               { addr, _ := l.r.LeaderWithID(); return string(addr) }

// Members implements cluster.Membership: the servers of the node's latest
// configuration, voters or not, in its order, with their addresses as the
// configuration writes them. hashicorp/raft keeps a copy of that configuration
// apart from its main loop (GetConfiguration in v1.8.0), so a call reads it
// without waiting on raft.
func (l *Leadership) Members() []cluster.Peer {
	future := l.r.GetConfiguration()
	if future.Error() != nil {
		return nil
	}
	servers := future.Configuration().Servers
	members := make([]cluster.Peer, 0, len(servers))
	for _, s := range servers {
		members = append(members, cluster.Peer{ID: string(s.ID), Address: string(s.Address)})
	}
	return members
}

// WhenLeader implements cluster.Leadership: while this node leads, it runs fn
// with a context canceled when the term ends, once WaitReady saw every committed
// write applied. If fn returns while the node still leads, WhenLeader calls it
// again, after another WaitReady, so fn must return only once its context ends:
// one that returns early runs again and again, each time after a barrier, an
// entry raft writes to its log. A WaitReady that fails while the node still
// leads is no leadership loss and ends nothing: WhenLeader logs one WARN,
// pauses, and waits again, as long as the term lasts. Only ctx ending, raft
// shutting down or an error of fn ends WhenLeader.
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
		if !l.awaitReady(leaderCtx) {
			finish(current)
			if ctx.Err() != nil {
				return nil
			}
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

// awaitReady waits, at the start of the term whose context is term, until
// WaitReady succeeds, and reports whether it did while the term lasts. A
// WaitReady that fails while the node still leads and the term lasts, a barrier
// that timed out for one, is tried again after one WARN and a pause of
// retryPause. The term ends with a leadership loss, raft shutting down, or the
// context of WhenLeader.
func (l *Leadership) awaitReady(term context.Context) bool {
	for {
		err := l.WaitReady(term)
		if term.Err() != nil || !l.AmLeader() {
			return false
		}
		if err == nil {
			return true
		}
		l.logger.Warn().Err(err).Dur("retry_in", l.retryPause).
			Msg("raft: the applied state is not ready at the start of a leadership term: waiting again")
		pause := time.NewTimer(l.retryPause)
		select {
		case <-term.Done():
			pause.Stop()
			return false
		case <-pause.C:
		}
	}
}
