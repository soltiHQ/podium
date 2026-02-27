// Package lifecycle implements a server.Runner that periodically checks agent liveness
//   - Transitions agents through status stages: (active → inactive → disconnected → deleted)
//
// Thresholds are expressed as multiples of each agent's heartbeat interval.
package lifecycle

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Runner is a server.Runner that periodically checks agent liveness.
type Runner struct {
	logger  zerolog.Logger
	cfg     Config
	store   storage.AgentStore
	stop    chan struct{}
	started atomic.Bool
}

// New creates a lifecycle runner.
func New(cfg Config, logger zerolog.Logger, store storage.AgentStore) (*Runner, error) {
	if store == nil {
		return nil, errors.New("lifecycle: store is nil")
	}
	cfg = cfg.withDefaults()
	return &Runner{
		logger: logger.With().Str("runner", cfg.Name).Logger(),
		cfg:    cfg,
		store:  store,
		stop:   make(chan struct{}),
	}, nil
}

// Name returns the runner name.
func (r *Runner) Name() string { return r.cfg.Name }

// Start runs the lifecycle check loop until Stop is called.
func (r *Runner) Start(_ context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return errors.New("lifecycle: already started")
	}

	ticker := time.NewTicker(r.cfg.TickInterval)
	defer ticker.Stop()

	r.logger.Debug().
		Dur("tick", r.cfg.TickInterval).
		Int("inactive", r.cfg.InactiveMultiplier).
		Int("disconnect", r.cfg.DisconnectMultiplier).
		Int("delete", r.cfg.DeleteMultiplier).
		Msg("lifecycle runner started")

	for {
		select {
		case <-ticker.C:
			r.tick()
		case <-r.stop:
			r.logger.Info().Msg("lifecycle runner stopped")
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
	var (
		ctx = context.Background()
		now = time.Now()

		res, err = r.store.ListAgents(ctx, nil, storage.ListOptions{
			Limit: storage.MaxListLimit,
		})
	)
	if err != nil {
		r.logger.Error().Err(err).Msg("tick: list agents failed")
		return
	}

	for _, a := range res.Items {
		if a == nil {
			continue
		}

		hb := a.HeartbeatInterval()
		if hb <= 0 {
			hb = r.cfg.DefaultHeartbeat
		}

		silence := now.Sub(a.LastSeenAt())
		switch {
		case silence > hb*time.Duration(r.cfg.DeleteMultiplier):
			if err = r.store.DeleteAgent(ctx, a.ID()); err != nil {
				r.logger.Warn().Err(err).Str("agent_id", a.ID()).Msg("tick: delete failed")
				continue
			}
			r.logger.Info().
				Str("agent_id", a.ID()).
				Dur("silence", silence).
				Msg("agent deleted (stale)")

		case silence > hb*time.Duration(r.cfg.DisconnectMultiplier):
			if a.Status() != kind.AgentStatusDisconnected {
				a.SetStatus(kind.AgentStatusDisconnected)

				if err = r.store.UpsertAgent(ctx, a); err != nil {
					r.logger.Warn().Err(err).Str("agent_id", a.ID()).Msg("tick: upsert disconnected failed")
					continue
				}
				r.logger.Info().
					Str("agent_id", a.ID()).
					Msg("agent → disconnected")
			}

		case silence > hb*time.Duration(r.cfg.InactiveMultiplier):
			if a.Status() != kind.AgentStatusInactive {
				a.SetStatus(kind.AgentStatusInactive)

				if err = r.store.UpsertAgent(ctx, a); err != nil {
					r.logger.Warn().Err(err).Str("agent_id", a.ID()).Msg("tick: upsert inactive failed")
					continue
				}
				r.logger.Info().
					Str("agent_id", a.ID()).
					Msg("agent → inactive")
			}
		}
	}
}
