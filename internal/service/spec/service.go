// Package spec implements task spec management use-cases:
//   - Paginated listing and retrieval
//   - Creation, update with version increment, and deletion
//   - Deployment (rollout creation for target agents)
//   - Rollout querying by spec.
package spec

import (
	"context"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Service provides task spec management operations.
type Service struct {
	logger zerolog.Logger
	store  storage.Storage
}

// New creates a new task spec service.
func New(store storage.Storage, logger zerolog.Logger) *Service {
	if store == nil {
		panic("spec.Service: store is nil")
	}
	return &Service{
		logger: logger.With().Str("service", "specs").Logger(),
		store:  store,
	}
}

// List returns a page of task specs matching the query.
func (s *Service) List(ctx context.Context, q ListQuery) (*Page, error) {
	res, err := s.store.ListSpecs(ctx, s.store.BuildSpecFilter(q.Criteria), storage.ListOptions{
		Limit:  service.NormalizeListLimit(q.Limit, defaultListLimit),
		Cursor: q.Cursor,
	})
	if err != nil {
		return nil, err
	}

	out := make([]*model.Spec, 0, len(res.Items))
	for _, ts := range res.Items {
		if ts == nil {
			continue
		}
		out = append(out, ts.Clone())
	}
	return &Page{
		Items:      out,
		NextCursor: res.NextCursor,
	}, nil
}

// Get returns a single spec by ID.
func (s *Service) Get(ctx context.Context, id string) (*model.Spec, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}
	ts, err := s.store.GetSpec(ctx, id)
	if err != nil {
		return nil, err
	}
	return ts.Clone(), nil
}

// Create persists a new spec. Rejects specs that carry the
// deletion-requested flag — a tombstoned ID can only transition to
// "finalised" (fully deleted), never be resurrected through Create.
func (s *Service) Create(ctx context.Context, ts *model.Spec) error {
	if ts == nil {
		return storage.ErrInvalidArgument
	}
	if ts.DeletionRequested() {
		return storage.ErrInvalidArgument
	}

	if err := s.store.UpsertSpec(ctx, ts); err != nil {
		return err
	}

	s.logger.Debug().Str("spec_id", ts.ID()).Msg("spec created")
	return nil
}

// Upsert persists changes to an existing task spec. `Version` bumps on
// every save (edits counter). `Generation` bumps only if the **runtime**
// fields — those that end up in `SpecToProto` — actually changed;
// metadata-only edits (rename, target-list) never rotate live tasks.
//
// `expectedVersion` is the optimistic-concurrency CAS token: the client
// sends the version it last read, and Upsert rejects with a
// [ConflictError] if the stored version has advanced in the meantime.
// Pass `0` to opt out of the check (background jobs with no rival
// writer); interactive UI flows should always pass the observed version.
//
// Save is not a deploy: rollouts stay where they are. The detail page
// surfaces the drift ("spec gen 2, rollouts at gen 1") and the user must
// click Deploy to apply.
func (s *Service) Upsert(ctx context.Context, ts *model.Spec, expectedVersion int) error {
	if ts == nil {
		return storage.ErrInvalidArgument
	}

	old, err := s.store.GetSpec(ctx, ts.ID())
	if err != nil {
		return err
	}

	// A spec that has been marked for deletion is a tombstone: the sync
	// runner is in the middle of tearing it down on agents and the
	// finalizer is waiting to drop the row. Any further Upsert would
	// resurrect runtime state on top of an in-flight uninstall — chaos.
	// Reject; the only legal transition is "finalized and gone".
	if old.DeletionRequested() {
		return storage.ErrInvalidArgument
	}

	// Optimistic-concurrency check. expectedVersion == 0 opts out;
	// interactive UI must always pass what the user's load() returned.
	if expectedVersion > 0 && old.Version() != expectedVersion {
		return &ConflictError{Expected: expectedVersion, Actual: old.Version()}
	}

	// The handler hands us a *model.Spec mutated from the current stored
	// instance, so `old` and `ts` can share pointer-equal kindConfig
	// maps. Compare before bumping version/generation: RuntimeEquals is
	// cheap and honest about which fields count as "runtime".
	runtimeChanged := !old.RuntimeEquals(ts)

	ts.IncrementVersion()
	if runtimeChanged {
		ts.BumpGeneration()
	}
	if err := s.store.UpsertSpec(ctx, ts); err != nil {
		return err
	}

	s.logger.Debug().
		Str("spec_id", ts.ID()).
		Int("version", ts.Version()).
		Int("generation", ts.Generation()).
		Bool("runtime_changed", runtimeChanged).
		Msg("spec updated")
	return nil
}

// Delete soft-deletes a spec: it flips `DeletionRequested` and marks
// every rollout `Intent=Uninstall, Pending`. The sync runner performs
// `DeleteTask` on each agent; the finalizer (run at the end of each
// sync tick) actually drops the spec row once all rollouts are gone.
//
// If there are no rollouts (nobody ever deployed), the spec is removed
// synchronously — no reason to tombstone.
func (s *Service) Delete(ctx context.Context, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}

	ts, err := s.store.GetSpec(ctx, id)
	if err != nil {
		// NotFound is fine — caller-visible idempotency: deleting a
		// missing spec is a no-op.
		return err
	}

	rolloutsRes, err := s.store.ListRollouts(ctx,
		s.store.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: id}),
		storage.ListOptions{Limit: storage.MaxListLimit},
	)
	if err != nil {
		return err
	}

	// Fast path: no rollouts, nothing to tear down.
	if len(rolloutsRes.Items) == 0 {
		if err := s.store.DeleteSpec(ctx, id); err != nil {
			return err
		}
		s.logger.Debug().Str("spec_id", id).Msg("spec deleted (no rollouts)")
		return nil
	}

	// Soft delete: tombstone the spec, queue Uninstall intents. The
	// sync runner finalizer will call `DeleteSpec` once rollouts drain.
	ts.MarkForDeletion()
	if err := s.store.UpsertSpec(ctx, ts); err != nil {
		return err
	}

	for _, r := range rolloutsRes.Items {
		if r == nil {
			continue
		}
		r.SetIntent(kind.RolloutIntentUninstall)
		r.MarkPending(ts.Version())
		if err := s.store.UpsertRollout(ctx, r); err != nil {
			return err
		}
	}

	s.logger.Debug().
		Str("spec_id", id).
		Int("pending_uninstalls", len(rolloutsRes.Items)).
		Msg("spec soft-deleted; rollouts queued for uninstall")
	return nil
}

// ForceDelete drops a spec and all its rollouts immediately, without
// waiting for agents to confirm task teardown. Use sparingly — any task
// still running on an agent becomes orphaned: the agent keeps running
// it until its own lifecycle ends or a human manually cancels it.
//
// Intended escape hatch for the case where a spec's uninstall rollouts
// are stuck (agent offline for days, retries exhausted) and the user
// explicitly accepts the orphan-task risk. The handler/UI must gate
// this behind a confirmation dialog.
func (s *Service) ForceDelete(ctx context.Context, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}

	if err := s.store.DeleteRolloutsBySpec(ctx, id); err != nil {
		return err
	}
	if err := s.store.DeleteSpec(ctx, id); err != nil {
		return err
	}
	s.logger.Warn().Str("spec_id", id).Msg("spec force-deleted; agent tasks may be orphaned")
	return nil
}

// Rollouts returns all rollouts matching the given criteria. The
// service owns the translation from domain-level criteria to the
// backend-specific filter — callers never import the storage backend.
func (s *Service) Rollouts(ctx context.Context, c storage.RolloutQueryCriteria) ([]*model.Rollout, error) {
	res, err := s.store.ListRollouts(ctx,
		s.store.BuildRolloutFilter(c),
		storage.ListOptions{Limit: storage.MaxListLimit},
	)
	if err != nil {
		return nil, err
	}

	out := make([]*model.Rollout, 0, len(res.Items))
	for _, r := range res.Items {
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// RolloutsBySpec is a thin wrapper over `Rollouts` that always filters
// by the given spec. Results are cloned so callers can mutate freely.
func (s *Service) RolloutsBySpec(ctx context.Context, specID string) ([]*model.Rollout, error) {
	if specID == "" {
		return nil, storage.ErrInvalidArgument
	}

	res, err := s.store.ListRollouts(ctx,
		s.store.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: specID}),
		storage.ListOptions{Limit: storage.MaxListLimit},
	)
	if err != nil {
		return nil, err
	}

	out := make([]*model.Rollout, 0, len(res.Items))
	for _, ss := range res.Items {
		if ss == nil {
			continue
		}
		out = append(out, ss.Clone())
	}
	return out, nil
}

// Deploy is the reconciler that maps the current desired state (spec
// targets + spec generation) onto the set of Rollout records. It is
// idempotent — clicking Deploy on a fully-synced spec is a no-op.
//
// For each agent we decide:
//
//   - Agent in `spec.Targets`, no rollout yet           → create, Intent=Install
//   - Agent in `spec.Targets`, rollout behind generation → Intent=Update
//   - Agent in `spec.Targets`, rollout at current gen    → leave (Noop)
//   - Agent NOT in `spec.Targets`, rollout exists        → Intent=Uninstall
//
// Deploying a spec flagged for deletion is rejected — the only valid
// transition from `DeletionRequested=true` is "finalized" (rows drain,
// finalizer removes the spec).
func (s *Service) Deploy(ctx context.Context, specID string) error {
	if specID == "" {
		return storage.ErrInvalidArgument
	}

	ts, err := s.store.GetSpec(ctx, specID)
	if err != nil {
		return err
	}
	if ts.DeletionRequested() {
		return storage.ErrInvalidArgument
	}

	targets := ts.Targets()

	// Validate every declared target exists before touching any rollout
	// state. Stops silent zombie rollouts that otherwise would churn
	// through sync-runner retries and end up stuck in Failed forever.
	// Pre-check is cheap (single GetAgent per target); rolling back
	// already-persisted rollouts halfway through is not.
	var missing []string
	for _, agentID := range targets {
		if _, err := s.store.GetAgent(ctx, agentID); err != nil {
			missing = append(missing, agentID)
		}
	}
	if len(missing) > 0 {
		s.logger.Warn().
			Str("spec_id", specID).
			Strs("missing_agents", missing).
			Msg("deploy rejected: unknown target agents")
		return &UnknownTargetsError{Agents: missing}
	}

	wanted := make(map[string]struct{}, len(targets))
	for _, id := range targets {
		wanted[id] = struct{}{}
	}

	rolloutsRes, err := s.store.ListRollouts(ctx,
		s.store.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: specID}),
		storage.ListOptions{Limit: storage.MaxListLimit},
	)
	if err != nil {
		return err
	}

	// Start with the existing rollouts keyed by agent — we'll mutate /
	// create / mark-uninstall below.
	existing := make(map[string]*model.Rollout, len(rolloutsRes.Items))
	for _, r := range rolloutsRes.Items {
		if r != nil {
			existing[r.AgentID()] = r
		}
	}

	var (
		install, update, uninstall, noop int
	)

	// Targets side: Install or Update or Noop.
	for _, agentID := range targets {
		if r, ok := existing[agentID]; ok {
			if r.ObservedGeneration() == ts.Generation() && r.Status() == kind.SyncStatusSynced {
				// Already synced at the current generation. Don't disturb.
				noop++
				continue
			}
			// Either behind generation or in a transient/failed state —
			// queue an update. Intent=Update is honest even when actual
			// task id is empty (the runner treats it like Install with
			// a pre-delete guarded by ActualTaskID != "").
			if r.ActualTaskID() == "" {
				r.SetIntent(kind.RolloutIntentInstall)
				install++
			} else {
				r.SetIntent(kind.RolloutIntentUpdate)
				update++
			}
			r.MarkPending(ts.Version())
			if err := s.store.UpsertRollout(ctx, r); err != nil {
				return err
			}
			continue
		}
		// Fresh rollout for a newly added target.
		r, err := model.NewRollout(specID, agentID, ts.Version())
		if err != nil {
			return err
		}
		// NewRollout defaults intent to Install; explicit for clarity.
		r.SetIntent(kind.RolloutIntentInstall)
		if err := s.store.UpsertRollout(ctx, r); err != nil {
			return err
		}
		install++
	}

	// Orphan side: agent removed from targets → queue Uninstall.
	for agentID, r := range existing {
		if _, kept := wanted[agentID]; kept {
			continue
		}
		r.SetIntent(kind.RolloutIntentUninstall)
		r.MarkPending(ts.Version())
		if err := s.store.UpsertRollout(ctx, r); err != nil {
			return err
		}
		uninstall++
	}

	s.logger.Debug().
		Str("spec_id", specID).
		Int("generation", ts.Generation()).
		Int("install", install).
		Int("update", update).
		Int("uninstall", uninstall).
		Int("noop", noop).
		Msg("deploy reconciled rollouts")
	return nil
}
