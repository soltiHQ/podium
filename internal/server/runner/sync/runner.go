package sync

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Runner is a server.Runner that periodically reconciles pending rollout
// records by pushing Specs to agents via the proxy pool.
//
// On each tick it:
//  1. Lists all rollouts with status pending, drift, or failed (under max retries).
//  2. For each, resolves the Spec and agent.
//  3. Obtains an AgentProxy from the pool and calls SubmitTask.
//  4. On success: marks the rollout as synced.
//  5. On failure: marks the rollout as failed (increments attempts).
type Runner struct {
	logger  zerolog.Logger
	cfg     Config
	store   storage.Storage
	pool    *proxy.Pool
	started atomic.Bool
	stop    chan struct{}
}

// New creates a sync runner.
func New(cfg Config, logger zerolog.Logger, store storage.Storage, pool *proxy.Pool) (*Runner, error) {
	if store == nil {
		return nil, errors.New("sync: store is nil")
	}
	if pool == nil {
		return nil, errors.New("sync: proxy pool is nil")
	}
	cfg = cfg.withDefaults()
	return &Runner{
		logger: logger.With().Str("runner", cfg.Name).Logger(),
		cfg:    cfg,
		store:  store,
		pool:   pool,
		stop:   make(chan struct{}),
	}, nil
}

// Name returns the runner name.
func (r *Runner) Name() string { return r.cfg.Name }

// Start runs the sync reconciliation loop until Stop is called.
func (r *Runner) Start(_ context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return errors.New("sync: already started")
	}

	ticker := time.NewTicker(r.cfg.TickInterval)
	defer ticker.Stop()

	r.logger.Info().
		Dur("tick", r.cfg.TickInterval).
		Int("max_retries", r.cfg.MaxRetries).
		Msg("sync runner started")

	for {
		select {
		case <-ticker.C:
			r.tick()
		case <-r.stop:
			r.logger.Info().Msg("sync runner stopped")
			return nil
		}
	}
}

// Stop signals the runner to exit.
func (r *Runner) Stop(_ context.Context) error {
	close(r.stop)
	return nil
}

func (r *Runner) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.PushTimeout)
	defer cancel()

	// List ALL rollouts and filter in-memory for actionable ones.
	res, err := r.store.ListRollouts(ctx, nil, storage.ListOptions{
		Limit: storage.MaxListLimit,
	})
	if err != nil {
		r.logger.Error().Err(err).Msg("tick: list rollouts failed")
		return
	}

	for _, ss := range res.Items {
		if ss == nil {
			continue
		}

		// Only process actionable states.
		switch ss.Status() {
		case kind.SyncStatusPending, kind.SyncStatusDrift:
			// always actionable
		case kind.SyncStatusFailed:
			if ss.Attempts() >= r.cfg.MaxRetries {
				continue // exhausted retries
			}
		default:
			continue // synced, unknown — skip
		}

		r.push(ctx, ss.ID(), ss.SpecID(), ss.AgentID())
	}
}

func (r *Runner) push(ctx context.Context, ssID, specID, agentID string) {
	// 1. Get Spec
	ts, err := r.store.GetSpec(ctx, specID)
	if err != nil {
		r.logger.Warn().Err(err).
			Str("spec_id", specID).
			Str("rollout_id", ssID).
			Msg("push: get spec failed")
		r.markFailed(ctx, ssID, "spec not found: "+err.Error())
		return
	}

	// 2. Get Agent (for endpoint info)
	ag, err := r.store.GetAgent(ctx, agentID)
	if err != nil {
		r.logger.Warn().Err(err).
			Str("agent_id", agentID).
			Str("rollout_id", ssID).
			Msg("push: get agent failed")
		r.markFailed(ctx, ssID, "agent not found: "+err.Error())
		return
	}

	// 3. Get proxy from pool
	ap, err := r.pool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion())
	if err != nil {
		r.logger.Warn().Err(err).
			Str("agent_id", agentID).
			Str("endpoint", ag.Endpoint()).
			Str("rollout_id", ssID).
			Msg("push: get proxy failed")
		r.markFailed(ctx, ssID, "proxy error: "+err.Error())
		return
	}

	// 4. Submit spec
	spec := ts.ToCreateSpec()
	err = ap.SubmitTask(ctx, proxy.TaskSubmission{Spec: spec})
	if err != nil {
		r.logger.Warn().Err(err).
			Str("agent_id", agentID).
			Str("spec_id", specID).
			Str("rollout_id", ssID).
			Msg("push: submit task failed")
		r.markFailed(ctx, ssID, "submit error: "+err.Error())
		return
	}

	// 5. Mark synced
	r.markSynced(ctx, ssID, ts.Version())

	r.logger.Info().
		Str("agent_id", agentID).
		Str("spec_id", specID).
		Int("version", ts.Version()).
		Msg("spec pushed to agent")
}

func (r *Runner) markSynced(ctx context.Context, ssID string, version int) {
	ss, err := r.store.GetRollout(ctx, ssID)
	if err != nil {
		r.logger.Error().Err(err).Str("rollout_id", ssID).Msg("markSynced: get failed")
		return
	}
	ss.MarkSynced(version)
	if err := r.store.UpsertRollout(ctx, ss); err != nil {
		r.logger.Error().Err(err).Str("rollout_id", ssID).Msg("markSynced: upsert failed")
	}
}

func (r *Runner) markFailed(ctx context.Context, ssID, errMsg string) {
	ss, err := r.store.GetRollout(ctx, ssID)
	if err != nil {
		r.logger.Error().Err(err).Str("rollout_id", ssID).Msg("markFailed: get failed")
		return
	}
	ss.MarkFailed(errMsg)
	if err := r.store.UpsertRollout(ctx, ss); err != nil {
		r.logger.Error().Err(err).Str("rollout_id", ssID).Msg("markFailed: upsert failed")
	}
}
