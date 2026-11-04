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

// TermFunc is the work a term task does once in each leadership term. It
// returns nil once the work is done.
//
// ctx is the leader context:
// it is canceled the moment this replica loses leadership or the server shuts down.
type TermFunc func(ctx context.Context) error

const (
	// termRetryPause is the pause after the first failure of a term task within a
	// term. It doubles after each further failure, up to termMaxRetryPause.
	termRetryPause    = time.Second
	termMaxRetryPause = time.Minute

	// What a term task logs: once its work of the term is done, once its term
	// ends before that, and for each failure it retries.
	termTaskDone  = "runner task done for this leadership term"
	termTaskEnded = "runner task ended with its leadership term: the next term runs it again"
	termTaskRetry = "runner task failed: retrying"
)

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

	// term is the task of a term task, nil for a periodic runner. retryPause and
	// maxRetryPause bound its pauses: termRetryPause and termMaxRetryPause.
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

// NewTermTask builds a leader-gated runner that runs task at the start of each
// leadership term of this replica, once the leadership is ready (see
// [cluster.Leadership.WhenLeader]), and then waits for the term to end; it
// never ticks. A task that fails while the term lasts logs one WARN and runs
// again after a pause that starts at a second and doubles after each failure,
// up to a minute. A task that fails as the term ends, or with a leadership error
// ([cluster.IsLeadershipError]), is not retried: one INFO says that the next
// term runs it again. A pause never delays Stop.
func NewTermTask(name string, leadership cluster.Leadership, logger zerolog.Logger, task TermFunc) *Runner {
	switch {
	case leadership == nil:
		panic("base: nil leadership")
	case task == nil:
		panic("base: nil term task")
	}
	return &Runner{
		name:          name,
		leadership:    leadership,
		term:          task,
		retryPause:    termRetryPause,
		maxRetryPause: termMaxRetryPause,
		logger:        logger.With().Str("runner", name).Logger(),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
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

// runTerm is one leadership term of a term task, whose context is ctx: it runs
// the task until the task succeeds or the term ends, retrying a failure after a
// pause while the term lasts, and returns once the term ended.
func (r *Runner) runTerm(ctx context.Context) {
	r.logger.Info().Msg("runner acquired leadership")
	if r.onLeader != nil {
		r.onLeader()
	}
	pause := r.retryPause
	for attempt := 1; ; attempt++ {
		err := r.term(ctx)
		if err == nil {
			r.logger.Info().Int("attempts", attempt).Msg(termTaskDone)
			break
		}
		if ctx.Err() == nil && !cluster.IsLeadershipError(err) {
			r.logger.Warn().Err(err).Int("attempt", attempt).Dur("retry_in", pause).Msg(termTaskRetry)
			if sleep(ctx, pause) {
				pause = min(2*pause, r.maxRetryPause)
				continue
			}
		}
		r.logger.Info().Err(err).Int("attempts", attempt).Msg(termTaskEnded)
		break
	}
	<-ctx.Done()
}

// sleep waits for d, and reports whether it did before ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
