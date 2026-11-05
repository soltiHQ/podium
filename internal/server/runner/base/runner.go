package base

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// TickFunc runs a single reconciliation pass.
//
// ctx is the leader context:
// it is canceled the moment this replica loses leadership or the server shuts down.
type TickFunc func(ctx context.Context)

// Runner is a leader-gated server.Runner.
//
// While leader, a runner from New invokes tick every interval, and a term task
// from NewTermTask runs its task once per leadership term;
// on followers it parks inside the leadership gate.
type Runner struct {
	name       string
	interval   time.Duration
	leadership cluster.Leadership
	tick       TickFunc
	logger     zerolog.Logger
	onLeader   func()

	// term is the task of a term task (term.go), nil for a periodic runner.
	// retryPause and maxRetryPause bound its pauses: termRetryPause and
	// termMaxRetryPause.
	term                      TermFunc
	retryPause, maxRetryPause time.Duration

	stop     chan struct{} // closed by Stop to request exit
	done     chan struct{} // closed when Start's loop has fully returned
	started  atomic.Bool
	stopOnce sync.Once
}

// New builds a periodic runner.
func New(name string, interval time.Duration, leadership cluster.Leadership, logger zerolog.Logger, tick TickFunc) *Runner {
	switch {
	case interval <= 0:
		panic("base: non-positive interval")
	case leadership == nil:
		panic("base: nil leadership")
	case tick == nil:
		panic("base: nil tick")
	}
	return &Runner{
		name:       name,
		interval:   interval,
		leadership: leadership,
		tick:       tick,
		logger:     logger.With().Str("runner", name).Logger(),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// Name returns the runner name.
func (r *Runner) Name() string { return r.name }

// OnLeader sets fn to run each time this replica takes leadership, before the
// first tick of that term, or the term task. A runner whose bookkeeping may have
// gone stale while another replica led renews it there. It must be called before
// Start.
func (r *Runner) OnLeader(fn func()) { r.onLeader = fn }

// Start runs the leader-gated tick loop, or the term task, until Stop is called
// or ctx is canceled.
func (r *Runner) Start(ctx context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	defer close(r.done)

	event := r.logger.Info()
	if r.term == nil {
		event = event.Dur("tick", r.interval)
	}
	event.Msg("runner started")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-runCtx.Done():
		}
	}()

	return r.leadership.WhenLeader(runCtx, func(leaderCtx context.Context) error {
		if r.term != nil {
			r.runTerm(leaderCtx)
			return nil
		}
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		r.logger.Info().Msg("runner acquired leadership")
		if r.onLeader != nil {
			r.onLeader()
		}
		for {
			select {
			case <-ticker.C:
				r.tick(leaderCtx)
			case <-leaderCtx.Done():
				return nil
			}
		}
	})
}

// Stop requests shutdown and blocks until the tick loop has fully drained or ctx expires, whichever comes first.
func (r *Runner) Stop(ctx context.Context) error {
	if !r.started.Load() {
		return nil
	}
	r.stopOnce.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
