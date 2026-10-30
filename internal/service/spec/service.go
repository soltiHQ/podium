package spec

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/internal/proxy"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
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

	return &Page{
		Items:      res.Items,
		NextCursor: res.NextCursor,
	}, nil
}

// Get returns a single spec by ID.
func (s *Service) Get(ctx context.Context, id string) (*model.Spec, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}
	return s.store.GetSpec(ctx, id)
}

// Create persists a new spec.
func (s *Service) Create(ctx context.Context, ts *model.Spec) error {
	if err := validateSpec(ts); err != nil {
		return err
	}
	if ts.DeletionRequested() {
		return storage.ErrInvalidArgument
	}

	if err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		_, err := tx.GetSpec(ctx, ts.ID())
		if err == nil {
			return storage.ErrAlreadyExists
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		return tx.UpsertSpec(ctx, ts)
	}); err != nil {
		return err
	}

	s.logger.Debug().Str("spec_id", ts.ID()).Msg("spec created")
	return nil
}

// Upsert persists changes to an existing task spec. `Version` bumps on every save (edits counter).
// `Generation` bumps only if the **runtime** fields - those that end up in `SpecToProto`: actually changed;
// metadata-only edits (rename, target-list) never rotate live tasks.
//
// `expectedVersion` is the optimistic-concurrency CAS token:
// the client sends the version it last read, and Upsert rejects with a [ConflictError] if the stored version has advanced in the meantime.
// Pass `0` to opt out of the check (background jobs with no rival writer); interactive UI flows should always pass the observed version.
func (s *Service) Upsert(ctx context.Context, ts *model.Spec, expectedVersion int) error {
	if err := validateSpec(ts); err != nil {
		return err
	}

	var runtimeChanged bool
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		old, err := tx.GetSpec(ctx, ts.ID())
		if err != nil {
			return err
		}

		if old.DeletionRequested() {
			return storage.ErrInvalidArgument
		}

		if expectedVersion > 0 && old.Version() != expectedVersion {
			return &ConflictError{Expected: expectedVersion, Actual: old.Version()}
		}
		runtimeChanged = !old.RuntimeEquals(ts)

		ts.SetVersion(old.Version())
		ts.SetGeneration(old.Generation())
		ts.SetCreatedAt(old.CreatedAt())
		ts.IncrementVersion()
		if runtimeChanged {
			ts.BumpGeneration()
		}
		return tx.UpsertSpec(ctx, ts)
	})
	if err != nil {
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

// Delete soft-deletes a spec:
// it flips `DeletionRequested` and marks every rollout `Intent=Uninstall, Pending`.
//
// The sync runner performs `DeleteTask` on each agent; the finalizer (run at the end of each sync tick)
// actually drops the spec row once all rollouts are gone.
//
// If there are no rollouts (nobody ever deployed), the spec is removed synchronously - no reason to tombstone.
func (s *Service) Delete(ctx context.Context, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}

	var (
		fastPath bool
		pending  int
	)
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		ts, err := tx.GetSpec(ctx, id)
		if err != nil {
			return err
		}
		rolloutsRes, err := storage.AllRollouts(ctx, tx, tx.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: id}))
		if err != nil {
			return err
		}

		if len(rolloutsRes) == 0 {
			fastPath = true
			return tx.DeleteSpec(ctx, id)
		}

		ts.MarkForDeletion()
		if err = tx.UpsertSpec(ctx, ts); err != nil {
			return err
		}
		for _, r := range rolloutsRes {
			if r == nil {
				continue
			}
			r.SetIntent(enum.RolloutIntentUninstall)
			r.MarkPending(ts.Generation())
			if err = tx.UpsertRollout(ctx, r); err != nil {
				return err
			}
		}
		pending = len(rolloutsRes)
		return nil
	})
	if err != nil {
		return err
	}

	if fastPath {
		s.logger.Debug().Str("spec_id", id).Msg("spec deleted (no rollouts)")
	} else {
		s.logger.Debug().Str("spec_id", id).Int("pending_uninstalls", pending).Msg("spec soft-deleted")
	}
	return nil
}

// ForceDelete drops a spec and all its rollouts immediately, without waiting for agents to confirm task teardown.
//
// Use sparingly - any task still running on an agent becomes orphaned:
// the agent keeps running it until its own lifecycle ends or a human manually cancels it.
func (s *Service) ForceDelete(ctx context.Context, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}

	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		if err := tx.DeleteRolloutsBySpec(ctx, id); err != nil {
			return err
		}
		return tx.DeleteSpec(ctx, id)
	})
	if err != nil {
		return err
	}
	s.logger.Warn().Str("spec_id", id).Msg("spec force-deleted; agent tasks may be orphaned")
	return nil
}

// Rollouts return all rollouts matching the given criteria.
// The service owns the translation from domain-level criteria to the backend-specific filter - callers never import the storage backend.
func (s *Service) Rollouts(ctx context.Context, c storage.RolloutQueryCriteria) ([]*model.Rollout, error) {
	res, err := storage.AllRollouts(ctx, s.store, s.store.BuildRolloutFilter(c))
	if err != nil {
		return nil, err
	}
	return res, nil
}

// RolloutsBySpec is a thin wrapper over `Rollouts` that always filters by the given spec.
func (s *Service) RolloutsBySpec(ctx context.Context, specID string) ([]*model.Rollout, error) {
	if specID == "" {
		return nil, storage.ErrInvalidArgument
	}

	res, err := storage.AllRollouts(ctx, s.store, s.store.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: specID}))
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Deploy is the reconciler that maps the current desired state (spec targets + spec generation) onto the set of Rollout records.
// It is idempotent: clicking Deploy on a fully-synced spec is a no-op.
//
// For each agent:
//   - Agent in `spec.Targets`, no rollout yet            → create, Intent=Install
//   - Agent in `spec.Targets`, rollout behind generation → Intent=Update
//   - Agent in `spec.Targets`, rollout at current gen    → leave (Noop)
//   - Agent NOT in `spec.Targets`, rollout exists        → Intent=Uninstall
//
// Deploying a spec flagged for deletion is rejected - the only valid transition from `DeletionRequested=true` is "finalized"
// (rows drain, finalizer removes the spec).
//
// `planTokens` are the plan tokens of the placements the caller reviewed (see [Service.PreviewPlacement]). Unless it
// is nil, the token of the plan recomputed inside the transaction must be one of them, else Deploy writes nothing and
// returns [ErrPlacementChanged]; an empty list names no plan. Pass nil to deploy the current placement unchecked (API
// clients that never preview).
func (s *Service) Deploy(ctx context.Context, specID string, planTokens []string) error {
	if specID == "" {
		return storage.ErrInvalidArgument
	}

	var (
		generation                       int
		install, update, uninstall, noop int
	)
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		ts, err := tx.GetSpec(ctx, specID)
		if err != nil {
			return err
		}
		if ts.DeletionRequested() {
			return storage.ErrInvalidArgument
		}
		generation = ts.Generation()
		plan, steps, err := loadPlacement(ctx, tx, ts, time.Now())
		if err != nil {
			return err
		}
		if planTokens != nil && !slices.Contains(planTokens, plan.Token) {
			return ErrPlacementChanged
		}
		missing, unsupported := blockedTargets(plan)
		if len(missing) > 0 {
			return &UnknownTargetsError{Agents: missing}
		}
		if len(unsupported) > 0 {
			return &UnsupportedTargetsError{Agents: unsupported}
		}

		for _, step := range steps {
			r := step.rollout
			switch step.action {
			case deployInstall:
				if r, err = model.NewRollout(specID, step.agentID, ts.Generation()); err != nil {
					return err
				}
				r.SetIntent(enum.RolloutIntentInstall)
				install++
			case deployUpdate:
				if r.ObservedGeneration() == ts.Generation() && r.Status() == enum.SyncStatusSynced {
					noop++
					continue
				}
				if r.ActualTaskID() == "" {
					r.SetIntent(enum.RolloutIntentInstall)
					install++
				} else {
					r.SetIntent(enum.RolloutIntentUpdate)
					update++
				}
				r.MarkPending(ts.Generation())
			case deployUninstall:
				r.SetIntent(enum.RolloutIntentUninstall)
				r.MarkPending(ts.Generation())
				uninstall++
			}
			if err = tx.UpsertRollout(ctx, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if unknown, ok := err.(*UnknownTargetsError); ok {
			s.logger.Warn().
				Str("spec_id", specID).
				Strs("missing_agents", unknown.Agents).
				Msg("deploy rejected: unknown target agents")
		}
		return err
	}

	s.logger.Debug().
		Str("spec_id", specID).
		Int("generation", generation).
		Int("install", install).
		Int("update", update).
		Int("uninstall", uninstall).
		Int("noop", noop).
		Msg("deploy reconciled rollouts")
	return nil
}

func validateSpec(ts *model.Spec) error {
	if ts == nil || strings.TrimSpace(ts.Name()) == "" {
		return fmt.Errorf("%w: spec name is required", storage.ErrInvalidArgument)
	}
	if _, err := proxy.SpecToProto(ts); err != nil {
		return fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	for k, v := range ts.TargetLabels() {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: target selector key/value required", storage.ErrInvalidArgument)
		}
	}
	return nil
}
