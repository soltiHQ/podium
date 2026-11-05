package base

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

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
	// WhenLeader runs this function again when it returns while the replica still
	// leads: the wait for the end of the term keeps the task to one run per term.
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
