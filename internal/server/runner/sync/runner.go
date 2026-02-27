// Package sync implements a server.Runner that reconciles pending rollouts
// by pushing specs to agents via the proxy pool:
//   - Lists actionable rollouts (pending, drift, failed under max retries)
//   - Resolves spec and agent, gets a proxy, calls SubmitTask
//   - Marks rollout synced on success, failed (with attempt increment) on error.
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
//  3. Gets an AgentProxy from the pool and calls "SubmitTask".
//  4. On success: marks the rollout as synced.
//  5. On failure: marks the rollout as failed (increment attempts).
type Runner struct {
	logger  zerolog.Logger
	cfg     Config
	store   storage.Storage
	pool    *proxy.Pool
	stop    chan struct{}
	started atomic.Bool
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

// Stop signals the runner to exit. Safe to call multiple times.
func (r *Runner) Stop(_ context.Context) error {
	if !r.started.Load() {
		return nil
	}
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	return nil
}

func (r *Runner) tick() {
	ctx := context.Background()

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

		switch ss.Status() {
		case kind.SyncStatusPending, kind.SyncStatusDrift:
		case kind.SyncStatusFailed:
			if ss.Attempts() >= r.cfg.MaxRetries {
				continue
			}
		default:
			continue
		}

		pushCtx, cancel := context.WithTimeout(ctx, r.cfg.PushTimeout)
		r.push(pushCtx, ss.ID(), ss.SpecID(), ss.AgentID())
		cancel()
	}
}

func (r *Runner) push(ctx context.Context, rID, specID, agentID string) {
	ts, err := r.store.GetSpec(ctx, specID)
	if err != nil {
		r.logger.Warn().Err(err).
			Str("rid", rID).
			Str("spec_id", specID).
			Msg("push: get spec failed")
		r.markFailed(ctx, rID, "spec not found: "+err.Error())
		return
	}
	ag, err := r.store.GetAgent(ctx, agentID)
	if err != nil {
		r.logger.Warn().Err(err).
			Str("rid", rID).
			Str("agent_id", agentID).
			Msg("push: get agent failed")
		r.markFailed(ctx, rID, "agent not found: "+err.Error())
		return
	}
	ap, err := r.pool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion())
	if err != nil {
		r.logger.Warn().Err(err).
			Str("rid", rID).
			Str("agent_id", agentID).
			Str("endpoint", ag.Endpoint()).
			Msg("push: get proxy failed")
		r.markFailed(ctx, rID, "proxy error: "+err.Error())
		return
	}

	err = ap.SubmitTask(ctx, proxy.TaskSubmission{Spec: ts.ToCreateSpec()})
	if err != nil {
		r.logger.Warn().Err(err).
			Str("rid", rID).
			Str("spec_id", specID).
			Str("agent_id", agentID).
			Msg("push: submit task failed")
		r.markFailed(ctx, rID, "submit error: "+err.Error())
		return
	}

	r.markSynced(ctx, rID, ts.Version())
	r.logger.Info().
		Str("spec_id", specID).
		Str("agent_id", agentID).
		Int("version", ts.Version()).
		Msg("spec pushed to agent")
}

func (r *Runner) markSynced(ctx context.Context, rID string, version int) {
	ss, err := r.store.GetRollout(ctx, rID)
	if err != nil {
		r.logger.Error().Err(err).Str("rid", rID).Msg("markSynced: get failed")
		return
	}

	ss.MarkSynced(version)
	if err = r.store.UpsertRollout(ctx, ss); err != nil {
		r.logger.Error().Err(err).Str("rid", rID).Msg("markSynced: upsert failed")
	}
}

func (r *Runner) markFailed(ctx context.Context, rID, errMsg string) {
	ss, err := r.store.GetRollout(ctx, rID)
	if err != nil {
		r.logger.Error().Err(err).Str("rid", rID).Msg("markFailed: get failed")
		return
	}

	ss.MarkFailed(errMsg)
	if err = r.store.UpsertRollout(ctx, ss); err != nil {
		r.logger.Error().Err(err).Str("rid", rID).Msg("markFailed: upsert failed")
	}
}
