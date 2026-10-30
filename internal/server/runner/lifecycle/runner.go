package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/server/runner/base"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// Runner periodically checks agent liveness (active → inactive → disconnected → deleted) while this replica is leader.
type Runner struct {
	*base.Runner

	hub    *event.Hub
	logger zerolog.Logger
	store  storage.Storage
	cfg    Config

	// startedAt is when this runner began watching. Silence is never counted from before it, so downtime is not held
	// against the agents. Zero means no grace.
	startedAt time.Time
	// clock replaces time.Now in tests; nil means time.Now.
	clock func() time.Time
}

// New creates a lifecycle runner.
func New(cfg Config, logger zerolog.Logger, store storage.Storage, hub *event.Hub, leadership cluster.Leadership) (*Runner, error) {
	if store == nil {
		return nil, fmt.Errorf("lifecycle: %w", storage.ErrNilStore)
	}
	if hub == nil {
		return nil, fmt.Errorf("lifecycle: %w", event.ErrNilHub)
	}
	if leadership == nil {
		return nil, fmt.Errorf("lifecycle: nil leadership")
	}
	cfg = cfg.withDefaults()

	r := &Runner{
		logger: logger.With().Str("runner", cfg.Name).Logger(),
		cfg:    cfg,
		store:  store,
		hub:    hub,
	}
	r.startedAt = r.now()
	r.Runner = base.New(cfg.Name, cfg.TickInterval, leadership, logger, r.tick)
	return r, nil
}

func (r *Runner) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func (r *Runner) tick(ctx context.Context) {
	now := r.now()
	items, err := storage.AllAgents(ctx, r.store, inmemory.NewAgentFilter().StaleAtBefore(now))
	if err != nil {
		r.logger.Error().Err(err).Msg("tick: list agents failed")
		return
	}
	var g errgroup.Group
	g.SetLimit(r.cfg.MaxConcurrency)
	for _, a := range items {
		if a == nil {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error { r.reconcile(ctx, now, a); return nil })
	}
	_ = g.Wait()
}

func (r *Runner) reconcile(ctx context.Context, now time.Time, observed *model.Agent) {
	var kind string
	var changed *model.Agent
	err := r.store.WithTx(ctx, func(tx storage.Storage) error {
		a, err := tx.GetAgent(ctx, observed.ID())
		if err != nil {
			return err
		}
		// Discovery may have refreshed this agent after the stale snapshot was taken.
		if !a.LastSeenAt().Equal(observed.LastSeenAt()) {
			return nil
		}
		hb := a.HeartbeatInterval()
		if hb <= 0 {
			hb = r.cfg.DefaultHeartbeat
		}
		// Count the silence from the later of the last sync and this runner's start: a control plane that was down
		// must not delete agents that had no chance to sync yet.
		since := a.LastSeenAt()
		if r.startedAt.After(since) {
			since = r.startedAt
		}
		silence := now.Sub(since)
		// Statuses rank active < inactive < disconnected and lifecycle never lowers one: the silence restarts at
		// zero after a restart, and only a sync makes an agent active again.
		switch {
		case silence > max(r.cfg.DeleteAfter, hb*time.Duration(r.cfg.DeleteMultiplier)):
			if err = tx.DeleteAgent(ctx, a.ID()); err != nil {
				return err
			}
			kind = event.AgentDeleted
		case silence > hb*time.Duration(r.cfg.DisconnectMultiplier):
			if a.Status() >= enum.AgentStatusDisconnected {
				return nil
			}
			a.MarkStatus(enum.AgentStatusDisconnected)
			if err = tx.UpsertAgent(ctx, a); err != nil {
				return err
			}
			kind = event.AgentDisconnected
		case silence > hb*time.Duration(r.cfg.InactiveMultiplier):
			if a.Status() >= enum.AgentStatusInactive {
				return nil
			}
			a.MarkStatus(enum.AgentStatusInactive)
			if err = tx.UpsertAgent(ctx, a); err != nil {
				return err
			}
			kind = event.AgentInactive
		default:
			return nil
		}
		changed = a
		return nil
	})
	if err != nil {
		r.logger.Warn().Err(err).Str("agent_id", observed.ID()).Msg("lifecycle reconcile failed")
		return
	}
	if changed != nil {
		r.hub.Record(kind, event.Payload{ID: changed.ID(), Name: changed.Name(), By: "lifecycle"})
		r.hub.Notify(event.RefreshAgents)
	}
}
