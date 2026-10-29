// Package history collects observed terminal runs on the leader without
// affecting desired-state reconciliation or the lifetime of SDK resources.
package history

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/server/runner/base"
	historysvc "github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/storage"
	"golang.org/x/sync/errgroup"
)

type agentWork struct {
	cursor  string
	pending []*model.Rollout
	queued  bool
}
type workEntry struct {
	id   string
	work *agentWork
}
type runProgress struct {
	task, agent  string
	complete     bool
	continuation historysvc.RunContinuation
	touched      time.Time
}
type Runner struct {
	*base.Runner
	cfg            Config
	logger         zerolog.Logger
	store          storage.Storage
	pool           *proxy.Pool
	svc            *historysvc.Service
	tickMu         sync.Mutex
	workMu         sync.Mutex
	agentCursor    string
	pendingAgents  []*model.Agent
	work           map[string]*agentWork
	maintenance    []workEntry
	progress       map[string]*runProgress
	progressCursor string
}

func New(cfg Config, logger zerolog.Logger, store storage.Storage, pool *proxy.Pool, svc *historysvc.Service, leadership cluster.Leadership) (*Runner, error) {
	if store == nil || pool == nil || svc == nil || leadership == nil {
		return nil, fmt.Errorf("history collector: missing dependency")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r := &Runner{cfg: cfg.WithDefaults(), logger: logger.With().Str("runner", "history").Logger(), store: store, pool: pool, svc: svc, work: map[string]*agentWork{}, progress: map[string]*runProgress{}}
	r.Runner = base.New("history", r.cfg.TickInterval, leadership, logger, r.tick)
	return r, nil
}

func (r *Runner) tick(parent context.Context) {
	r.tickMu.Lock()
	defer r.tickMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, r.cfg.CycleTimeout)
	defer cancel()
	if _, err := r.svc.Prune(ctx); err != nil {
		r.logger.Warn().Err(err).Msg("archive: retention failed")
	}
	r.pruneWork(ctx)
	r.pruneProgress(ctx)
	if len(r.pendingAgents) == 0 {
		page, err := r.store.ListAgents(ctx, nil, storage.ListOptions{Cursor: r.agentCursor, Limit: r.cfg.MaxAgentsPerCycle})
		if err != nil {
			r.agentCursor = ""
			r.logger.Warn().Err(err).Msg("archive: agent page failed")
			return
		}
		r.agentCursor = page.NextCursor
		r.pendingAgents = page.Items
	}
	var group errgroup.Group
	slots := make(chan struct{}, r.cfg.MaxConcurrency)
schedule:
	for processed := 0; processed < r.cfg.MaxAgentsPerCycle && len(r.pendingAgents) > 0; processed++ {
		if ctx.Err() != nil {
			break
		}
		// Acquire capacity before consuming the identity. An expired cycle must
		// not dequeue an agent which never got a live collection slot.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break schedule
		}
		if ctx.Err() != nil {
			<-slots
			break
		}
		// Retain any unstarted identities for the next bounded cycle.
		agentID := r.pendingAgents[0].ID()
		r.pendingAgents[0] = nil
		r.pendingAgents = r.pendingAgents[1:]
		if len(r.pendingAgents) == 0 {
			r.pendingAgents = nil
		}
		group.Go(func() error { defer func() { <-slots }(); r.collectAgent(ctx, agentID); return nil })
	}
	_ = group.Wait()
}

func (r *Runner) collectAgent(ctx context.Context, agentID string) {
	agent, err := r.store.GetAgent(ctx, agentID)
	if err != nil {
		r.workMu.Lock()
		delete(r.work, agentID)
		r.workMu.Unlock()
		return
	}
	if agent.Endpoint() == "" {
		return
	}
	token := ""
	if credential, err := r.store.GetAgentCredential(ctx, agentID); err == nil {
		token = credential.Token()
	} else if !errors.Is(err, storage.ErrNotFound) {
		return
	}
	remote, err := r.pool.Get(agent.Endpoint(), agent.EndpointType(), agent.APIVersion(), token)
	if err != nil {
		r.logger.Debug().Err(err).Str("agent_id", agentID).Msg("archive: agent proxy unavailable")
		return
	}
	r.workMu.Lock()
	work := r.work[agentID]
	if work == nil {
		work = &agentWork{}
		r.work[agentID] = work
	}
	r.workMu.Unlock()
	defer r.retainWork(agentID, work)
	if len(work.pending) == 0 {
		page, err := r.store.ListRollouts(ctx, r.store.BuildRolloutFilter(storage.RolloutQueryCriteria{AgentID: agentID}), storage.ListOptions{Cursor: work.cursor, Limit: r.cfg.PageSize})
		if err != nil {
			work.cursor = ""
			r.logger.Debug().Err(err).Str("agent_id", agentID).Msg("archive: rollout page failed")
			return
		}
		work.cursor = page.NextCursor
		work.pending = page.Items
	}
	opts := historysvc.DefaultCaptureOptions()
	opts.PageSize = uint32(r.cfg.PageSize)
	opts.MaxPages = r.cfg.MaxPages
	for len(work.pending) > 0 && ctx.Err() == nil {
		rolloutID := work.pending[0].ID()
		work.pending[0] = nil
		work.pending = work.pending[1:]
		if len(work.pending) == 0 {
			work.pending = nil
		}
		rollout, err := r.store.GetRollout(ctx, rolloutID)
		if err != nil || rollout.AgentID() != agentID || rollout.ActualTaskID() == "" {
			continue
		}
		req := historysvc.CaptureRequest{AgentID: agentID, AgentName: agent.Name(), TaskName: rollout.ActualTaskID(), SpecID: rollout.SpecID()}
		if spec, err := r.store.GetSpec(ctx, rollout.SpecID()); err == nil {
			req.SpecName = spec.Name()
		}
		opts.Continuation = r.resumeProgress(rolloutID, req)
		opts.KnownComplete = r.completedSnapshot(rolloutID, req)
		result, err := r.svc.Capture(ctx, req, remote, opts)
		if err != nil {
			if errors.Is(err, historysvc.ErrSnapshotChanged) {
				r.rememberProgress(rolloutID, req, nil)
			}
			r.logger.Debug().Err(err).Str("agent_id", agentID).Str("task", req.TaskName).Msg("archive: capture unavailable; retained history remains")
			continue
		}
		if !result.Partial {
			r.rememberComplete(rolloutID, req, result.TaskUID, result.SourceResourceVersion)
		} else {
			r.rememberProgress(rolloutID, req, result.Continuation)
		}
		if result.Partial {
			r.logger.Warn().Str("agent_id", agentID).Str("task", req.TaskName).Msg("archive: capture reached page or payload bound; archive is partial")
		}
	}
}

// Only unfinished rollout pages need per-agent state. Deleted agents no longer
// appear in inventory, so a bounded FIFO sweep checks those retained identities
// separately without restarting an active agent's continuation.
func (r *Runner) retainWork(agentID string, work *agentWork) {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	if len(work.pending) == 0 && work.cursor == "" {
		delete(r.work, agentID)
		return
	}
	if !work.queued {
		work.queued = true
		r.maintenance = append(r.maintenance, workEntry{id: agentID, work: work})
	}
}

func (r *Runner) pruneWork(ctx context.Context) {
	r.workMu.Lock()
	count := len(r.maintenance)
	r.workMu.Unlock()
	if count > r.cfg.MaxAgentsPerCycle {
		count = r.cfg.MaxAgentsPerCycle
	}
	for i := 0; i < count && ctx.Err() == nil; i++ {
		r.workMu.Lock()
		entry := r.maintenance[0]
		id := entry.id
		r.maintenance[0] = workEntry{}
		r.maintenance = r.maintenance[1:]
		work := r.work[id]
		r.workMu.Unlock()
		if work == nil || work != entry.work {
			continue
		}
		_, err := r.store.GetAgent(ctx, id)
		r.workMu.Lock()
		if errors.Is(err, storage.ErrNotFound) {
			delete(r.work, id)
		} else {
			r.maintenance = append(r.maintenance, entry)
		}
		r.workMu.Unlock()
	}
}

func (r *Runner) resumeProgress(id string, req historysvc.CaptureRequest) *historysvc.RunContinuation {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	p := r.progress[id]
	if p == nil {
		return nil
	}
	if p.task != req.TaskName || p.agent != req.AgentID {
		delete(r.progress, id)
		return nil
	}
	if p.complete {
		return nil
	}
	copy := p.continuation
	return &copy
}

func (r *Runner) rememberProgress(id string, req historysvc.CaptureRequest, continuation *historysvc.RunContinuation) {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	if continuation == nil {
		delete(r.progress, id)
		return
	}
	if r.progress[id] == nil && len(r.progress) >= r.cfg.MaxRunContinuations {
		oldest := ""
		for key, value := range r.progress {
			if oldest == "" || value.touched.Before(r.progress[oldest].touched) || (value.touched.Equal(r.progress[oldest].touched) && key < oldest) {
				oldest = key
			}
		}
		delete(r.progress, oldest)
		r.logger.Warn().Str("rollout_id", oldest).Msg("archive: continuation cache bound reached; evicted capture may restart and history is partial")
	}
	r.progress[id] = &runProgress{task: req.TaskName, agent: req.AgentID, continuation: *continuation, touched: time.Now()}
}

// The continuation map has a hard cap. Its bounded sorted sweep also removes
// orphan rollouts/agents which cannot appear in the next live inventory page.
func (r *Runner) pruneProgress(ctx context.Context) {
	r.workMu.Lock()
	keys := make([]string, 0, len(r.progress))
	for key := range r.progress {
		keys = append(keys, key)
	}
	r.workMu.Unlock()
	sort.Strings(keys)
	start := sort.SearchStrings(keys, r.progressCursor)
	if start < len(keys) && keys[start] == r.progressCursor {
		start++
	}
	if start >= len(keys) {
		start = 0
	}
	count := len(keys)
	if count > r.cfg.MaxAgentsPerCycle {
		count = r.cfg.MaxAgentsPerCycle
	}
	for i := 0; i < count && ctx.Err() == nil; i++ {
		id := keys[(start+i)%len(keys)]
		r.progressCursor = id
		r.workMu.Lock()
		p := r.progress[id]
		r.workMu.Unlock()
		if p == nil {
			continue
		}
		rollout, err := r.store.GetRollout(ctx, id)
		remove := errors.Is(err, storage.ErrNotFound) || (err == nil && (rollout.AgentID() != p.agent || rollout.ActualTaskID() != p.task))
		if !remove && err == nil {
			_, err = r.store.GetAgent(ctx, p.agent)
			remove = errors.Is(err, storage.ErrNotFound)
		}
		if remove {
			r.workMu.Lock()
			delete(r.progress, id)
			r.workMu.Unlock()
		}
	}
}

func (r *Runner) completedSnapshot(id string, req historysvc.CaptureRequest) *historysvc.SnapshotIdentity {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	p := r.progress[id]
	if p == nil || !p.complete || p.task != req.TaskName || p.agent != req.AgentID {
		return nil
	}
	return &historysvc.SnapshotIdentity{TaskUID: p.continuation.TaskUID, ResourceVersion: p.continuation.ResourceVersion}
}

func (r *Runner) rememberComplete(id string, req historysvc.CaptureRequest, uid, rv string) {
	if uid == "" || rv == "" {
		return
	}
	r.rememberProgress(id, req, &historysvc.RunContinuation{TaskUID: uid, ResourceVersion: rv})
	r.workMu.Lock()
	if p := r.progress[id]; p != nil {
		p.complete = true
	}
	r.workMu.Unlock()
}
