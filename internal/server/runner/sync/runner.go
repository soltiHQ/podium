package sync

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
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
	Get(agentID, endpoint string, epType enum.EndpointType, apiVersion enum.APIVersion, token string) (proxy.AgentProxy, error)
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

	// clock replaces time.Now in tests; nil means time.Now.
	clock func() time.Time
	// backoffs holds back the calls to agents that left calls unanswered, and
	// incomplete says, for each agent whose tasks could not be listed completely
	// when last tried, why. Both live in memory only and are used by the tick
	// goroutine only, before the workers start and after they are done.
	backoffs   map[string]agentBackoff
	incomplete map[string]string
}

// agentBackoff holds back the calls to one agent.
type agentBackoff struct {
	failures int       // unanswered calls since the agent last answered
	until    time.Time // no call to the agent before this
	// The agent row when the backoff started: a later row that shows a
	// reconnect (model.Agent.ReconnectedSince) clears the backoff.
	status enum.AgentStatus
	uptime int64
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
	// It writes history too, when it captures a task before it deletes it.
	r.OnLeader(history.Resume)
	return r, nil
}

func (r *Runner) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func (r *Runner) tick(ctx context.Context) {
	r.reconcileRollouts(ctx)
	r.finalizeDeletedSpecs(ctx)
}

// Stages order an agent's rollouts in a tick: an observation never ends the
// agent's turn ahead of its pushes, and an uninstall that needs no call never
// waits for the agent.
const (
	stageLocal   = iota // uninstall of a task the agent never got: no call
	stagePush           // install, update, uninstall
	stageObserve        // check that an installed task still exists
)

func stage(ss *model.Rollout) int {
	switch {
	case ss.Intent() == enum.RolloutIntentUninstall && ss.ActualTaskID() == "":
		return stageLocal
	case ss.Intent() == enum.RolloutIntentNoop:
		return stageObserve
	default:
		return stagePush
	}
}

// outcome is what reconciling a rollout learned about its agent.
type outcome int

const (
	noCall     outcome = iota // the agent was not called
	answered                  // the agent answered, an error status included
	unanswered                // a call got no answer
)

// called sorts a finished agent call by unreachable.
func called(ctx context.Context, err error) outcome {
	if unreachable(ctx, err) {
		return unanswered
	}
	return answered
}

// agentTurn is one agent's work in a tick, and what its worker learned.
type agentTurn struct {
	agentID  string
	rollouts []string  // in turn order, see planTurns
	calls    int       // index of the first rollout that may need a call
	observe  int       // index of the first observation; len(rollouts) if none
	answered bool      // the agent answered a call
	silentAt time.Time // when a call went unanswered; zero if none did
	listed   listing   // what listing the agent's tasks came to
}

// listing is what listing an agent's tasks came to in a turn. Its zero value
// says nothing: the agent was not listed, or went silent, or the tick was
// cancelled.
type listing struct {
	complete bool
	cause    string // why not, when the agent answered and the listing still failed
	err      error  // what lies behind the cause, if there is more to say: the agent's error, or the numbers
}

// Why the tasks of an agent that answers cannot be listed completely.
const (
	causeTooLarge   = "page refused as too large even at limit 1"
	causeBudget     = "turn budget used before the last page"
	causeCursor     = "agent repeated a cursor"
	causeSnapshot   = "pages name different snapshots"
	causeAnswer     = "page answered with an error"
	causeMoreTasks  = "agent returned more tasks than asked"
	causeLongCursor = "agent returned a cursor that is too long"
	causeListSize   = "listing too large"
)

// reconcileRollouts dispatches every due rollout by intent, one worker per agent.
// A worker takes its agent's rollouts in turn order, each call within its own
// PushTimeout, starts no call once the turn has used PushTimeout of wall time,
// and stops at the first call the agent leaves unanswered. The agent's calls
// then back off until the backoff passes or the agent row shows a reconnect;
// uninstalls that need no call go on regardless. The observations, which come
// last in the turn, are one listing of the agent's tasks: see observeAgent.
func (r *Runner) reconcileRollouts(ctx context.Context) {
	items, err := storage.AllRollouts(ctx, r.store, nil)
	if err != nil {
		r.logger.Error().Err(err).Msg("tick: list rollouts failed")
		return
	}

	now := r.now()
	listed := make(map[string]bool)
	for _, ss := range items {
		if ss != nil {
			listed[ss.AgentID()] = true
		}
	}
	r.forgetAgents(ctx, listed, now)

	due := make([]*model.Rollout, 0, len(items))
	for _, ss := range items {
		if ss == nil {
			continue
		}
		if retryDue(ss, now, r.cfg) && (stage(ss) == stageLocal || !now.Before(r.backoffs[ss.AgentID()].until)) {
			due = append(due, ss)
		}
	}
	turns := r.planTurns(due)

	var g errgroup.Group
	g.SetLimit(r.cfg.MaxConcurrency)

	for _, turn := range turns {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			r.reconcileAgent(ctx, turn)
			return nil
		})
	}
	_ = g.Wait()
	for _, turn := range turns {
		r.noteAgent(ctx, turn)
	}
}

// forgetAgents drops what the runner keeps in memory about agents without
// rollouts: their backoff holds nothing back, and a listing that failed is news
// no more. It also drops the backoff of an agent that is held back while its row
// shows a reconnect since the backoff started: an agent that came back is called
// again at once.
func (r *Runner) forgetAgents(ctx context.Context, listed map[string]bool, now time.Time) {
	for agentID, b := range r.backoffs {
		if !listed[agentID] || (now.Before(b.until) && r.reconnected(ctx, agentID, b)) {
			delete(r.backoffs, agentID)
		}
	}
	for agentID := range r.incomplete {
		if !listed[agentID] {
			delete(r.incomplete, agentID)
		}
	}
}

// reconnected reports whether the agent row shows a reconnect since b started.
// A row that cannot be read shows none.
func (r *Runner) reconnected(ctx context.Context, agentID string, b agentBackoff) bool {
	ag, err := r.store.GetAgent(ctx, agentID)
	return err == nil && ag.ReconnectedSince(b.status, b.uptime)
}

// planTurns groups the due rollouts into one turn per agent, the agents in the
// stage order of their first rollout, and orders each turn: the uninstalls that
// need no call, in listing order; then the pushes by LastPushedAt, which only
// MarkFailed sets (rollouts that never failed first, then the oldest failure);
// then the observations. A synced rollout without a task has nothing to look
// up: it is left out, and an agent with nothing else is not called.
func (r *Runner) planTurns(due []*model.Rollout) []*agentTurn {
	slices.SortStableFunc(due, func(a, b *model.Rollout) int { return cmp.Compare(stage(a), stage(b)) })
	var (
		agents  []string
		byStage = make(map[string]*[3][]*model.Rollout)
	)
	for _, ss := range due {
		if stage(ss) == stageObserve && ss.ActualTaskID() == "" {
			continue
		}
		stages, ok := byStage[ss.AgentID()]
		if !ok {
			stages = new([3][]*model.Rollout)
			byStage[ss.AgentID()] = stages
			agents = append(agents, ss.AgentID())
		}
		stages[stage(ss)] = append(stages[stage(ss)], ss)
	}

	turns := make([]*agentTurn, 0, len(agents))
	for _, agentID := range agents {
		var (
			stages  = byStage[agentID]
			local   = stages[stageLocal]
			push    = stages[stagePush]
			observe = stages[stageObserve]
		)
		slices.SortStableFunc(push, func(a, b *model.Rollout) int { return a.LastPushedAt().Compare(b.LastPushedAt()) })
		turn := &agentTurn{agentID: agentID, calls: len(local), observe: len(local) + len(push)}
		for _, ss := range slices.Concat(local, push, observe) {
			turn.rollouts = append(turn.rollouts, ss.ID())
		}
		turns = append(turns, turn)
	}
	return turns
}

// reconcileAgent works through one agent's turn. Once the turn has used
// PushTimeout of wall time it starts no more calls: the rest waits for the next
// tick and records nothing. The first call the agent leaves unanswered ends the
// turn too: every rollout after that one needs the agent. The observations, last
// in the turn, are one listing of the agent's tasks, which ends the turn.
func (r *Runner) reconcileAgent(ctx context.Context, turn *agentTurn) {
	start := r.now()
	for i, rolloutID := range turn.rollouts {
		if ctx.Err() != nil {
			return
		}
		if i >= turn.calls && r.now().Sub(start) >= r.cfg.PushTimeout {
			return
		}
		if i == turn.observe {
			r.observeAgent(ctx, turn, start)
			return
		}
		pushCtx, cancel := context.WithTimeout(ctx, r.cfg.PushTimeout)
		result := r.reconcile(pushCtx, rolloutID)
		cancel()
		if ctx.Err() != nil {
			return // a cancelled tick says nothing about the agent
		}
		switch result {
		case answered:
			turn.answered = true
		case unanswered:
			turn.silentAt = r.now()
			return
		}
	}
}

// noteAgent updates the agent's backoff after its turn, and notes how listing
// its tasks went. An answered call clears the backoff. A call left unanswered
// holds the agent's calls back for backoff(n), n being the unanswered calls since
// the agent last answered, and records the agent row as the backoff starts.
func (r *Runner) noteAgent(ctx context.Context, turn *agentTurn) {
	r.noteListing(turn)

	switch {
	case !turn.silentAt.IsZero():
		failures := 1
		if !turn.answered {
			failures += r.backoffs[turn.agentID].failures
		}
		if r.backoffs == nil {
			r.backoffs = make(map[string]agentBackoff)
		}
		b := agentBackoff{failures: failures, until: turn.silentAt.Add(r.cfg.backoff(failures))}
		b.status, b.uptime = r.agentRow(ctx, turn.agentID)
		r.backoffs[turn.agentID] = b
		r.logger.Debug().
			Str("agent_id", turn.agentID).
			Int("failures", failures).
			Time("until", b.until).
			Msg("agent unreachable; its calls wait for the backoff")
	case turn.answered:
		delete(r.backoffs, turn.agentID)
	}
}

// noteListing logs when the tasks of an agent that answers can no longer be
// listed completely, with the cause, and when they can be again, naming the
// latest cause: once for each change, not on every tick the listing stays as it
// was. A cause that changes while the listing stays incomplete is kept for the
// line that ends it, not logged. A turn that listed nothing, got no answer or was
// cancelled leaves the state as it was.
func (r *Runner) noteListing(turn *agentTurn) {
	was, incomplete := r.incomplete[turn.agentID]
	switch l := turn.listed; {
	case l.complete && incomplete:
		delete(r.incomplete, turn.agentID)
		r.logger.Info().Str("agent_id", turn.agentID).Str("was", was).Msg("agent tasks listed completely again; its rollouts are observed")
	case l.cause != "":
		if r.incomplete == nil {
			r.incomplete = make(map[string]string)
		}
		r.incomplete[turn.agentID] = l.cause
		if !incomplete {
			r.logger.Warn().Err(l.err).Str("agent_id", turn.agentID).Str("cause", l.cause).Msg("agent tasks cannot be listed completely; its rollouts are not observed")
		}
	}
}

// agentRow returns the status and uptime of the agent row as a backoff starts.
// A missing row counts as an agent that is not active, as it does for Upsert,
// so the agent syncing again is a reconnect. A row that cannot be read counts
// as active with no uptime, which no later row shows a reconnect against.
func (r *Runner) agentRow(ctx context.Context, agentID string) (enum.AgentStatus, int64) {
	ag, err := r.store.GetAgent(ctx, agentID)
	switch {
	case err == nil:
		return ag.Status(), ag.UptimeSeconds()
	case errors.Is(err, storage.ErrNotFound):
		return enum.AgentStatusDisconnected, 0
	default:
		return enum.AgentStatusActive, 0
	}
}

// retryDue reports whether a tick should reconcile the rollout. A failed rollout
// waits backoff(attempts) after its last push; after MaxRetries attempts it waits
// for its agent to reconnect or for a new Deploy.
func retryDue(r *model.Rollout, now time.Time, cfg Config) bool {
	if r.Status() != enum.SyncStatusFailed {
		return true
	}
	return r.Attempts() < cfg.MaxRetries && !now.Before(r.LastPushedAt().Add(cfg.backoff(r.Attempts())))
}

// reconcile loads the rollout fresh (to pick up any changes since tick started) and dispatches by intent.
// Observations are not reconciled one by one, see observeAgent: a rollout that
// turned into one since the tick started waits for the next tick.
func (r *Runner) reconcile(ctx context.Context, rolloutID string) outcome {
	ss, err := r.store.GetRollout(ctx, rolloutID)
	if err != nil {
		r.logger.Warn().Err(err).Str("rid", rolloutID).Msg("reconcile: load rollout failed")
		return noCall
	}

	switch ss.Intent() {
	case enum.RolloutIntentUninstall:
		return r.reconcileUninstall(ctx, ss)
	case enum.RolloutIntentInstall, enum.RolloutIntentUpdate:
		return r.reconcileSubmit(ctx, ss)
	}
	return noCall
}

func (r *Runner) reconcileUninstall(ctx context.Context, ss *model.Rollout) outcome {
	if ss.ActualTaskID() == "" {
		if err := r.completeUninstall(ctx, ss); err != nil {
			r.logger.Error().Err(err).Str("rid", ss.ID()).Msg("reconcile/uninstall: delete rollout failed")
			return noCall
		}
		r.hub.Notify(event.RefreshSpecs)
		return noCall
	}

	ap, ok := r.getProxy(ctx, ss)
	if !ok {
		return noCall
	}

	r.captureBeforeDelete(ctx, ss, ap)

	if err := ap.DeleteTask(ctx, ss.ActualTaskID()); err != nil && !isNotFound(err) {
		r.markFailed(ctx, ss, "delete task: "+err.Error())
		return called(ctx, err)
	}

	if err := r.completeUninstall(ctx, ss); err != nil {
		r.logger.Error().Err(err).Str("rid", ss.ID()).Msg("reconcile/uninstall: delete rollout failed")
		return answered
	}
	r.logger.Info().
		Str("rid", ss.ID()).
		Str("spec_id", ss.SpecID()).
		Str("agent_id", ss.AgentID()).
		Msg("rollout uninstalled")
	r.notifyTaskChange()
	return answered
}

func (r *Runner) reconcileSubmit(ctx context.Context, ss *model.Rollout) outcome {
	ts, ap, protoSpec, ok := r.prepareSubmit(ctx, ss)
	if !ok {
		return noCall
	}

	wasInstalled := ss.ActualTaskID() != ""
	newID, err := ap.ApplyTask(ctx, proxy.TaskSubmission{Spec: protoSpec})
	if err != nil {
		r.markFailed(ctx, ss, "apply task: "+err.Error())
		return called(ctx, err)
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
	return answered
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
	ap, err := r.pool.Get(ag.ID(), ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
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

// notifyTaskChange tells the pages of specs, and the pages of an agent's tasks and
// detail, that a rollout changed in a way they show: a push applied, an uninstall
// done or a lost task queued for a reinstall. The agent pages listen for
// agent_update besides their poll, and discovery no longer sends it on every
// heartbeat. The hub coalesces the pulses of a kind, so a burst of them costs the
// pages two fetches.
func (r *Runner) notifyTaskChange() {
	r.hub.Notify(event.RefreshSpecs)
	r.hub.Notify(event.RefreshAgents)
}

// writeContext gives a state write its own stateWriteTimeout budget. It keeps the
// push context's values but not its cancellation, so the outcome of a call that
// used up the push deadline is still written.
func writeContext(pushCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(pushCtx), stateWriteTimeout)
}

// completeSubmit preserves desired-state changes made while an agent call was in flight.
// In particular, a concurrent deletion still needs to uninstall the task just returned.
func (r *Runner) completeSubmit(ctx context.Context, attempted *model.Rollout, generation int, taskID string) {
	ctx, cancel := writeContext(ctx)
	defer cancel()
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
	r.notifyTaskChange()
}

func (r *Runner) completeUninstall(ctx context.Context, attempted *model.Rollout) error {
	ctx, cancel := writeContext(ctx)
	defer cancel()
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
	// A cancelled tick (shutdown, lost leadership) is no failure of the rollout.
	// An expired push deadline is DeadlineExceeded and is recorded.
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	// The message is the words of the runner and, after them, the text of an agent's
	// error. The proxy cuts that text, but the words come on top of it: the rollout
	// and the event keep the whole message cut to the same limit.
	errMsg = proxy.CapAgentText(errMsg)
	ctx, cancel := writeContext(ctx)
	defer cancel()
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

// unreachable reports whether a failed agent call went unanswered: the push
// deadline passed, the HTTP request got no response, or the call timed out or
// lost its connection. An HTTP status the agent sent back, 4xx or 5xx, is always
// an answer. errkind.Unavailable does not count: agents answer 429, 503 and gRPC
// resource exhausted with it, so a refused gRPC connection, which gRPC also
// reports as Unavailable, cannot be told apart from an answer.
func unreachable(ctx context.Context, err error) bool {
	switch {
	case err == nil, errors.Is(err, proxy.ErrUnexpectedStatus):
		return false
	case ctx.Err() != nil, errors.Is(err, proxy.ErrRequest), errkind.Classify(err) == errkind.DeadlineExceeded:
		return true
	}
	// net.Error covers timeouts, context.DeadlineExceeded included, and refused
	// or reset connections.
	var netErr net.Error
	return errors.As(err, &netErr)
}

// observeAgent detects lost agent state after restart, without rotating terminal
// tasks that still exist: it lists the agent's tasks once, and queues each
// synced rollout of the turn whose task is not among them for a reinstall.
// Recovery reuses the stable manifest name and current intent. An agent whose
// row, credential or endpoint cannot be resolved is not called, and an
// incomplete listing records nothing on the rollouts, see listTasks.
func (r *Runner) observeAgent(ctx context.Context, turn *agentTurn, start time.Time) {
	ag, err := r.store.GetAgent(ctx, turn.agentID)
	if err != nil {
		return
	}
	token, err := r.agentToken(ctx, ag.ID())
	if err != nil {
		return
	}
	ap, err := r.pool.Get(ag.ID(), ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
	if err != nil {
		return
	}
	tasks, listed := r.listTasks(ctx, ap, turn, start)
	if !listed {
		return
	}
	for _, rolloutID := range turn.rollouts[turn.observe:] {
		if ctx.Err() != nil {
			return
		}
		r.recoverMissing(ctx, rolloutID, tasks)
	}
}

// taskListLimit is the page size a listing starts at: the most an agent serves.
const taskListLimit = 1000

// What an agent may make a listing cost, whatever it answers. A page with more
// tasks than were asked for, or a cursor longer than maxCursorBytes, is an answer
// no agent gives: the proxy refuses the first before it builds the tasks, and the
// runner checks both itself. A listing that would hold more than maxListedTasks
// task IDs or maxListedIDBytes bytes of them, or take more than maxListedPages
// calls, refused pages included, is not followed further: the runner keeps neither
// without bound, and the longest task ID the proxy lets through (proxy.MaxTaskIDBytes)
// times the count is more bytes than maxListedIDBytes allows, so the count alone
// does not bound them. Each leaves the listing incomplete, see listTasks.
const (
	maxCursorBytes   = 4096
	maxListedTasks   = 65536
	maxListedIDBytes = 32 << 20
	maxListedPages   = 1024
)

// listTasks lists every task of the turn's agent, a page at a time and with no
// filter, so that a task is there whatever its phase, as it was to GetTask. Each
// page is a call of its own, within PushTimeout, and starts only while the turn is
// within its budget. A page starts at taskListLimit tasks. One that the proxy
// refuses as too large is asked again from the same cursor with half the limit,
// down to one task, and the smaller limit holds for the rest of the turn. Only a
// listing that reaches the last page is complete. One that stops short reports
// false, and no task may be judged missing by it: the budget is used, a task is
// too large to list, a page fails, a page holds more tasks than it was asked for
// (the proxy refuses it before it builds the tasks, as ErrTooManyItems, and the
// runner counts them again), a cursor is longer than maxCursorBytes, the listing
// would hold more than maxListedTasks task IDs or maxListedIDBytes bytes of them,
// or take more than maxListedPages calls (a refused page is a call), the agent
// returns a cursor it returned before, the pages name different snapshots (a
// resource version that differs from the first one named; a page without one is
// let through) or the tick is cancelled. A page answers or goes unanswered as any
// call does, a refusal and an error status included: the turn notes it, and
// turn.listed says why a listing is incomplete.
func (r *Runner) listTasks(ctx context.Context, ap proxy.AgentProxy, turn *agentTurn, start time.Time) (map[string]struct{}, bool) {
	var (
		tasks   = make(map[string]struct{})
		cursors = make(map[string]struct{})
		cursor  string
		version string
		limit   = taskListLimit
		idBytes int
	)
	for call := 0; ; call++ {
		if call >= maxListedPages {
			turn.listed = listing{cause: causeListSize, err: fmt.Errorf("more than %d pages", maxListedPages)}
			return nil, false
		}
		if call > 0 && r.now().Sub(start) >= r.cfg.PushTimeout {
			turn.listed = listing{cause: causeBudget}
			return nil, false
		}
		callCtx, cancel := context.WithTimeout(ctx, r.cfg.PushTimeout)
		resp, err := ap.ListTasks(callCtx, proxy.TaskFilter{Limit: limit, Cursor: cursor})
		result := called(callCtx, err)
		cancel()
		if ctx.Err() != nil {
			return nil, false // a cancelled tick says nothing about the agent
		}
		switch result {
		case answered:
			turn.answered = true
		case unanswered:
			turn.silentAt = r.now()
			return nil, false
		}
		switch {
		case errors.Is(err, proxy.ErrResponseTooLarge) && limit > 1:
			limit /= 2
			continue
		case errors.Is(err, proxy.ErrResponseTooLarge):
			turn.listed = listing{cause: causeTooLarge, err: err}
			return nil, false
		case errors.Is(err, proxy.ErrTooManyItems):
			turn.listed = listing{cause: causeMoreTasks, err: err}
			return nil, false
		case err != nil:
			turn.listed = listing{cause: causeAnswer, err: err}
			return nil, false
		}
		if resp.ResourceVersion != "" {
			if version != "" && resp.ResourceVersion != version {
				turn.listed = listing{cause: causeSnapshot}
				return nil, false
			}
			version = resp.ResourceVersion
		}
		// The proxy refuses such a page before it builds the tasks, see above; this
		// check is the runner's own.
		if len(resp.Tasks) > limit {
			turn.listed = listing{cause: causeMoreTasks, err: fmt.Errorf("%d tasks for a limit of %d", len(resp.Tasks), limit)}
			return nil, false
		}
		if len(resp.NextCursor) > maxCursorBytes {
			turn.listed = listing{cause: causeLongCursor, err: fmt.Errorf("a cursor of %d bytes, over %d", len(resp.NextCursor), maxCursorBytes)}
			return nil, false
		}
		for _, task := range resp.Tasks {
			tasks[task.ID] = struct{}{}
			idBytes += len(task.ID) // per occurrence: an agent that repeats an ID is charged again
			if len(tasks) > maxListedTasks {
				turn.listed = listing{cause: causeListSize, err: fmt.Errorf("more than %d task IDs", maxListedTasks)}
				return nil, false
			}
			if idBytes > maxListedIDBytes {
				turn.listed = listing{cause: causeListSize, err: fmt.Errorf("more than %d bytes of task IDs", maxListedIDBytes)}
				return nil, false
			}
		}
		if resp.NextCursor == "" {
			turn.listed = listing{complete: true}
			return tasks, true
		}
		if _, repeated := cursors[resp.NextCursor]; repeated {
			turn.listed = listing{cause: causeCursor}
			return nil, false
		}
		cursors[resp.NextCursor] = struct{}{}
		cursor = resp.NextCursor
	}
}

// recoverMissing queues the rollout for a reinstall if its task is not among the
// tasks of its agent's complete listing. It loads the rollout fresh, as the
// listing may have taken long: one that is no longer a synced rollout with a
// task is left alone.
func (r *Runner) recoverMissing(ctx context.Context, rolloutID string, tasks map[string]struct{}) {
	attempted, err := r.store.GetRollout(ctx, rolloutID)
	if err != nil {
		r.logger.Warn().Err(err).Str("rid", rolloutID).Msg("observe: load rollout failed")
		return
	}
	if attempted.Intent() != enum.RolloutIntentNoop || attempted.ActualTaskID() == "" {
		return
	}
	if _, ok := tasks[attempted.ActualTaskID()]; ok {
		return
	}
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	changed := false
	err = r.store.WithTx(writeCtx, func(tx storage.Storage) error {
		current, err := tx.GetRollout(writeCtx, attempted.ID())
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
		return tx.UpsertRollout(writeCtx, current)
	})
	if err != nil {
		r.logger.Warn().Err(err).Str("rid", attempted.ID()).Msg("observe: recovery failed")
	}
	if changed {
		r.notifyTaskChange()
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
