package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/server/runner/base"
	historysvc "github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// proxyGetter is the small subset of *proxy.Pool that the sync runner actually needs.
type proxyGetter interface {
	Get(endpoint string, epType enum.EndpointType, apiVersion enum.APIVersion, token string) (proxy.AgentProxy, error)
}

// Runner periodically reconciles Rollout records against live state on agents.
type Runner struct {
	*base.Runner

	pool    proxyGetter
	hub     *event.Hub
	history *historysvc.Service

	logger zerolog.Logger
	store  storage.Storage
	cfg    Config
}

// New creates a sync runner.
func New(cfg Config, logger zerolog.Logger, store storage.Storage, pool *proxy.Pool, hub *event.Hub, history *historysvc.Service, leadership cluster.Leadership) (*Runner, error) {
	if store == nil {
		return nil, fmt.Errorf("sync: %w", storage.ErrNilStore)
	}
	if pool == nil {
		return nil, fmt.Errorf("sync: %w", proxy.ErrNilPool)
	}
	if hub == nil {
		return nil, fmt.Errorf("sync: %w", event.ErrNilHub)
	}
	if history == nil {
		return nil, fmt.Errorf("sync: nil history service")
	}
	if leadership == nil {
		return nil, fmt.Errorf("sync: nil leadership")
	}

	cfg = cfg.withDefaults()
	r := &Runner{
		logger:  logger.With().Str("runner", cfg.Name).Logger(),
		store:   store,
		pool:    pool,
		cfg:     cfg,
		hub:     hub,
		history: history,
	}
	r.Runner = base.New(cfg.Name, cfg.TickInterval, leadership, logger, r.tick)
	return r, nil
}

func (r *Runner) tick(ctx context.Context) {
	r.reconcileRollouts(ctx)
	r.finalizeDeletedSpecs(ctx)
}

// reconcileRollouts picks up every actionable rollout and dispatches it by intent.
func (r *Runner) reconcileRollouts(ctx context.Context) {
	items, err := storage.AllRollouts(ctx, r.store, nil)
	if err != nil {
		r.logger.Error().Err(err).Msg("tick: list rollouts failed")
		return
	}

	var g errgroup.Group
	g.SetLimit(r.cfg.MaxConcurrency)

	for _, ss := range items {
		if ss == nil {
			continue
		}
		if ss.Intent() != enum.RolloutIntentNoop && ss.Status() == enum.SyncStatusFailed && ss.Attempts() >= r.cfg.MaxRetries {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		rolloutID := ss.ID()
		g.Go(func() error {
			pushCtx, cancel := context.WithTimeout(ctx, r.cfg.PushTimeout)
			defer cancel()
			r.reconcile(pushCtx, rolloutID)
			return nil
		})
	}
	_ = g.Wait()
}

// reconcile loads the rollout fresh (to pick up any changes since tick started) and dispatches by intent.
func (r *Runner) reconcile(ctx context.Context, rolloutID string) {
	ss, err := r.store.GetRollout(ctx, rolloutID)
	if err != nil {
		r.logger.Warn().Err(err).Str("rid", rolloutID).Msg("reconcile: load rollout failed")
		return
	}

	switch ss.Intent() {
	case enum.RolloutIntentUninstall:
		r.reconcileUninstall(ctx, ss)
	case enum.RolloutIntentInstall, enum.RolloutIntentUpdate:
		r.reconcileSubmit(ctx, ss)
	case enum.RolloutIntentNoop:
		r.observe(ctx, ss)
		return
	}
}

func (r *Runner) reconcileUninstall(ctx context.Context, ss *model.Rollout) {
	if ss.ActualTaskID() == "" {
		if err := r.completeUninstall(ctx, ss); err != nil {
			r.logger.Error().Err(err).Str("rid", ss.ID()).Msg("reconcile/uninstall: delete rollout failed")
			return
		}
		r.hub.Notify(event.RefreshSpecs)
		return
	}

	ap, ok := r.getProxy(ctx, ss)
	if !ok {
		return
	}

	r.captureBeforeDelete(ctx, ss, ap)

	if err := ap.DeleteTask(ctx, ss.ActualTaskID()); err != nil && !isNotFound(err) {
		r.markFailed(ctx, ss, "delete task: "+err.Error())
		return
	}

	if err := r.completeUninstall(ctx, ss); err != nil {
		r.logger.Error().Err(err).Str("rid", ss.ID()).Msg("reconcile/uninstall: delete rollout failed")
		return
	}
	r.logger.Info().
		Str("rid", ss.ID()).
		Str("spec_id", ss.SpecID()).
		Str("agent_id", ss.AgentID()).
		Msg("rollout uninstalled")
	r.hub.Notify(event.RefreshSpecs)
}

func (r *Runner) reconcileSubmit(ctx context.Context, ss *model.Rollout) {
	ts, ap, protoSpec, ok := r.prepareSubmit(ctx, ss)
	if !ok {
		return
	}

	wasInstalled := ss.ActualTaskID() != ""
	newID, err := ap.ApplyTask(ctx, proxy.TaskSubmission{Spec: protoSpec})
	if err != nil {
		r.markFailed(ctx, ss, "apply task: "+err.Error())
		return
	}

	r.completeSubmit(ctx, ss, ts.Generation(), newID)
	verb := "installed"
	if wasInstalled {
		verb = "updated"
	}
	r.logger.Info().
		Str("rid", ss.ID()).
		Str("spec_id", ss.SpecID()).
		Str("agent_id", ss.AgentID()).
		Str("task_id", newID).
		Int("generation", ts.Generation()).
		Msgf("rollout %s", verb)
}

// prepareSubmit loads spec + agent + proxy + proto conversion.
// Any failure at this stage calls markFailed and returns ok=false.
func (r *Runner) prepareSubmit(ctx context.Context, ss *model.Rollout) (*model.Spec, proxy.AgentProxy, *taskv1.TaskManifest, bool) {
	ts, err := r.store.GetSpec(ctx, ss.SpecID())
	if err != nil {
		r.markFailed(ctx, ss, "spec not found: "+err.Error())
		return nil, nil, nil, false
	}

	if ts.DeletionRequested() {
		return nil, nil, nil, false
	}
	if ts.Generation() != ss.DesiredGeneration() {
		r.markFailed(ctx, ss, "spec changed since deployment; deploy the current generation to continue")
		return nil, nil, nil, false
	}
	ap, ok := r.getProxy(ctx, ss)
	if !ok {
		return nil, nil, nil, false
	}

	protoSpec, err := proxy.SpecToProto(ts)
	if err != nil {
		r.markFailed(ctx, ss, "spec conversion: "+err.Error())
		return nil, nil, nil, false
	}

	return ts, ap, protoSpec, true
}

// getProxy resolves an AgentProxy for the rollout's target agent.
func (r *Runner) getProxy(ctx context.Context, ss *model.Rollout) (proxy.AgentProxy, bool) {
	ag, err := r.store.GetAgent(ctx, ss.AgentID())
	if err != nil {
		r.markFailed(ctx, ss, "agent not found: "+err.Error())
		return nil, false
	}
	token, err := r.agentToken(ctx, ss.AgentID())
	if err != nil {
		r.markFailed(ctx, ss, "agent credential lookup: "+err.Error())
		return nil, false
	}
	ap, err := r.pool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
	if err != nil {
		r.markFailed(ctx, ss, "proxy error: "+err.Error())
		return nil, false
	}
	return ap, true
}

// Missing credentials mean auth is disabled or the agent is not enrolled.
// Storage failures must not silently turn authenticated calls into anonymous ones.
func (r *Runner) agentToken(ctx context.Context, agentID string) (string, error) {
	c, err := r.store.GetAgentCredential(ctx, agentID)
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return c.Token(), nil
}

// finalizeDeletedSpecs removes any spec with DeletionRequested=true whose last rollout has drained.
// finalizeDeletedSpecs atomically checks that a tombstone has no remaining rollouts.
func (r *Runner) finalizeDeletedSpecs(ctx context.Context) {
	specs, err := storage.AllSpecs(ctx, r.store, nil)
	if err != nil {
		r.logger.Error().Err(err).Msg("finalize: list specs failed")
		return
	}
	for _, ts := range specs {
		if !ts.DeletionRequested() {
			continue
		}
		deleted := false
		err := r.store.WithTx(ctx, func(tx storage.Storage) error {
			current, err := tx.GetSpec(ctx, ts.ID())
			if errors.Is(err, storage.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if !current.DeletionRequested() {
				return nil
			}
			page, err := tx.ListRollouts(ctx, tx.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: ts.ID()}), storage.ListOptions{Limit: 1})
			if err != nil {
				return err
			}
			if len(page.Items) > 0 {
				return nil
			}
			if err = tx.DeleteSpec(ctx, ts.ID()); err != nil {
				return err
			}
			deleted = true
			return nil
		})
		if err != nil {
			r.logger.Error().Err(err).Str("spec_id", ts.ID()).Msg("finalize failed")
		}
		if deleted {
			r.hub.Notify(event.RefreshSpecs)
		}
	}
}

// completeSubmit preserves desired-state changes made while an agent call was in flight.
// In particular, a concurrent deletion still needs to uninstall the task just returned.
func (r *Runner) completeSubmit(ctx context.Context, attempted *model.Rollout, generation int, taskID string) {
	err := r.store.WithTx(ctx, func(tx storage.Storage) error {
		current, err := tx.GetRollout(ctx, attempted.ID())
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		current.SetActualTaskID(taskID)
		ts, err := tx.GetSpec(ctx, current.SpecID())
		if err != nil {
			return err
		}
		if ts.DeletionRequested() || current.Intent() == enum.RolloutIntentUninstall {
			current.SetIntent(enum.RolloutIntentUninstall)
			current.MarkPending(current.DesiredGeneration())
		} else if current.DesiredGeneration() != generation {
			current.SetIntent(enum.RolloutIntentUpdate)
			current.MarkPending(current.DesiredGeneration())
		} else {
			current.MarkSynced(generation)
		}
		return tx.UpsertRollout(ctx, current)
	})
	if err != nil {
		r.logger.Error().Err(err).Str("rid", attempted.ID()).Msg("complete submit failed")
		return
	}
	r.hub.Notify(event.RefreshSpecs)
}

func (r *Runner) completeUninstall(ctx context.Context, attempted *model.Rollout) error {
	return r.store.WithTx(ctx, func(tx storage.Storage) error {
		current, err := tx.GetRollout(ctx, attempted.ID())
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Intent() == enum.RolloutIntentUninstall && current.ActualTaskID() == attempted.ActualTaskID() {
			return tx.DeleteRollout(ctx, current.ID())
		}
		// A target was re-added during the delete. Keep the latest intent and reinstall.
		if current.ActualTaskID() == attempted.ActualTaskID() {
			current.SetActualTaskID("")
			current.SetIntent(enum.RolloutIntentInstall)
			current.MarkPending(current.DesiredGeneration())
			return tx.UpsertRollout(ctx, current)
		}
		return nil
	})
}

func (r *Runner) markFailed(ctx context.Context, attempted *model.Rollout, errMsg string) {
	changed := false
	err := r.store.WithTx(ctx, func(tx storage.Storage) error {
		current, err := tx.GetRollout(ctx, attempted.ID())
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !current.UpdatedAt().Equal(attempted.UpdatedAt()) {
			return nil
		}
		current.MarkFailed(errMsg)
		changed = true
		return tx.UpsertRollout(ctx, current)
	})
	if err != nil {
		r.logger.Error().Err(err).Str("rid", attempted.ID()).Msg("mark failed: upsert failed")
		return
	}
	if !changed {
		return
	}
	var specName string
	if ts, err := r.store.GetSpec(ctx, attempted.SpecID()); err == nil {
		specName = ts.Name()
	}
	r.hub.Record(event.SyncFailed, event.Payload{ID: attempted.SpecID(), Name: specName, Detail: errMsg, By: "sync"})
	r.hub.Notify(event.RefreshSpecs)
}
func isNotFound(err error) bool { return errkind.Classify(err) == errkind.NotFound }

// observe detects lost agent state after restart, without rotating terminal tasks
// that still exist. Recovery reuses the stable manifest name and current intent.
func (r *Runner) observe(ctx context.Context, attempted *model.Rollout) {
	if attempted.ActualTaskID() == "" {
		return
	}
	ag, err := r.store.GetAgent(ctx, attempted.AgentID())
	if err != nil {
		return
	}
	token, err := r.agentToken(ctx, ag.ID())
	if err != nil {
		return
	}
	ap, err := r.pool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
	if err != nil {
		return
	}
	_, err = ap.GetTask(ctx, attempted.ActualTaskID())
	if !isNotFound(err) {
		return
	}
	changed := false
	err = r.store.WithTx(ctx, func(tx storage.Storage) error {
		current, err := tx.GetRollout(ctx, attempted.ID())
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !current.UpdatedAt().Equal(attempted.UpdatedAt()) || current.Intent() != enum.RolloutIntentNoop {
			return nil
		}
		current.SetActualTaskID("")
		current.SetIntent(enum.RolloutIntentInstall)
		current.MarkPending(current.DesiredGeneration())
		changed = true
		return tx.UpsertRollout(ctx, current)
	})
	if err != nil {
		r.logger.Warn().Err(err).Str("rid", attempted.ID()).Msg("observe: recovery failed")
	}
	if changed {
		r.hub.Notify(event.RefreshSpecs)
	}
}

// captureBeforeDelete records only already terminal observations. It neither
// cancels nor waits for the running task and does not block normal deletion on
// an unavailable/malformed source. A small child deadline reserves time for
// the existing delete operation within the parent's reconciliation budget.
func (r *Runner) captureBeforeDelete(ctx context.Context, ss *model.Rollout, ap proxy.AgentProxy) {
	if r.history == nil {
		return
	} // Narrow test runners may omit production services.
	budget := 2 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if available := time.Until(deadline) / 3; available < budget {
			budget = available
		}
	}
	if budget <= 0 {
		return
	}
	captureCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	req := historysvc.CaptureRequest{AgentID: ss.AgentID(), TaskName: ss.ActualTaskID(), SpecID: ss.SpecID()}
	if agent, err := r.store.GetAgent(captureCtx, ss.AgentID()); err == nil {
		req.AgentName = agent.Name()
	}
	if spec, err := r.store.GetSpec(captureCtx, ss.SpecID()); err == nil {
		req.SpecName = spec.Name()
	}
	result, err := r.history.Capture(captureCtx, req, ap, historysvc.DefaultCaptureOptions())
	if err != nil {
		r.logger.Warn().Err(err).Str("rid", ss.ID()).Msg("archive: pre-delete capture failed; deletion proceeds and history may be partial")
	} else if result.Partial {
		r.logger.Warn().Str("rid", ss.ID()).Msg("archive: pre-delete capture bounded; history may be partial")
	}
}
