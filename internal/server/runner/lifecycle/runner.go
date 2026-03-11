// Package lifecycle implements a server.Runner that periodically checks agent liveness
//   - Transitions agents through status stages: (active → inactive → disconnected → deleted)
//
// Thresholds are expressed as multiples of each agent's heartbeat interval.
package lifecycle

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
)

// Runner is a server.Runner that periodically checks agent liveness.
type Runner struct {
	hub *event.Hub

	logger zerolog.Logger
	store  storage.AgentStore
	cfg    Config

	stop    chan struct{}
	started atomic.Bool
}

// New creates a lifecycle runner.
func New(cfg Config, logger zerolog.Logger, store storage.AgentStore, hub *event.Hub) (*Runner, error) {
	if store == nil {
		return nil, fmt.Errorf("lifecycle: %w", storage.ErrNilStore)
	}
	if hub == nil {
		return nil, fmt.Errorf("lifecycle: %w", event.ErrNilHub)
	}
	cfg = cfg.withDefaults()
	return &Runner{
		logger: logger.With().Str("runner", cfg.Name).Logger(),
		cfg:    cfg,
		store:  store,
		hub:    hub,
		stop:   make(chan struct{}),
	}, nil
}

// Name returns the runner name.
func (r *Runner) Name() string { return r.cfg.Name }

// Start runs the lifecycle check loop until Stop is called.
func (r *Runner) Start(_ context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}

	ticker := time.NewTicker(r.cfg.TickInterval)
	defer ticker.Stop()

	r.logger.Debug().
		Dur("tick", r.cfg.TickInterval).
		Int("inactive", r.cfg.InactiveMultiplier).
		Int("disconnect", r.cfg.DisconnectMultiplier).
		Int("delete", r.cfg.DeleteMultiplier).
		Int("max_concurrency", r.cfg.MaxConcurrency).
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

	var g errgroup.Group
	g.SetLimit(r.cfg.MaxConcurrency)

	for _, a := range res.Items {
		if a == nil {
			continue
		}

		g.Go(func() error {
			r.reconcile(ctx, now, a)
			return nil
		})
	}
	if err = g.Wait(); err != nil {
		r.logger.Error().Err(err).Msg("tick: reconcile failed")
	}
}

func (r *Runner) reconcile(ctx context.Context, now time.Time, a *model.Agent) {
	hb := a.HeartbeatInterval()
	if hb <= 0 {
		hb = r.cfg.DefaultHeartbeat
	}

	silence := now.Sub(a.LastSeenAt())
	switch {
	case silence > hb*time.Duration(r.cfg.DeleteMultiplier):
		if err := r.store.DeleteAgent(ctx, a.ID()); err != nil {
			r.logger.Warn().Err(err).Str("agent_id", a.ID()).Msg("reconcile: delete failed")
			return
		}
		r.logger.Info().
			Str("agent_id", a.ID()).
			Dur("silence", silence).
			Msg("agent deleted (stale)")

		r.hub.Record(event.AgentDeleted, event.Payload{ID: a.ID(), Name: a.Name()})
		r.hub.Notify(htmx.AgentUpdate)

	case silence > hb*time.Duration(r.cfg.DisconnectMultiplier):
		if a.Status() != kind.AgentStatusDisconnected {
			a.SetStatus(kind.AgentStatusDisconnected)

			if err := r.store.UpsertAgent(ctx, a); err != nil {
				r.logger.Warn().Err(err).Str("agent_id", a.ID()).Msg("reconcile: upsert disconnected failed")
				return
			}
			r.logger.Info().
				Str("agent_id", a.ID()).
				Msg("agent → disconnected")

			r.hub.Record(event.AgentDisconnected, event.Payload{ID: a.ID(), Name: a.Name()})
			r.hub.Notify(htmx.AgentUpdate)
		}

	case silence > hb*time.Duration(r.cfg.InactiveMultiplier):
		if a.Status() != kind.AgentStatusInactive {
			a.SetStatus(kind.AgentStatusInactive)

			if err := r.store.UpsertAgent(ctx, a); err != nil {
				r.logger.Warn().Err(err).Str("agent_id", a.ID()).Msg("reconcile: upsert inactive failed")
				return
			}
			r.logger.Info().
				Str("agent_id", a.ID()).
				Msg("agent → inactive")

			r.hub.Record(event.AgentInactive, event.Payload{ID: a.ID(), Name: a.Name()})
			r.hub.Notify(htmx.AgentUpdate)
		}
	}
}
