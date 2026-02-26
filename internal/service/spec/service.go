// Package spec implements task spec management use-cases:
//   - Paginated listing and retrieval
//   - Creation, update with version increment, and deletion
//   - Deployment (rollout creation for target agents)
//   - Rollout querying by spec.
package spec

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Service provides task spec management operations.
type Service struct {
	store storage.Storage
}

// New creates a new task spec service.
func New(store storage.Storage) *Service {
	if store == nil {
		panic("spec.Service: store is nil")
	}
	return &Service{store: store}
}

// List returns a page of task specs matching the query.
func (s *Service) List(ctx context.Context, q ListQuery) (*Page, error) {
	res, err := s.store.ListSpecs(ctx, q.Filter, storage.ListOptions{
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

// Create persists a new spec.
func (s *Service) Create(ctx context.Context, ts *model.Spec) error {
	if ts == nil {
		return storage.ErrInvalidArgument
	}
	return s.store.UpsertSpec(ctx, ts)
}

// Upsert persists changes to an existing task spec and increments its version.
func (s *Service) Upsert(ctx context.Context, ts *model.Spec) error {
	if ts == nil {
		return storage.ErrInvalidArgument
	}

	if _, err := s.store.GetSpec(ctx, ts.ID()); err != nil {
		return err
	}
	ts.IncrementVersion()
	return s.store.UpsertSpec(ctx, ts)
}

// Delete removes a task spec and all associated rollouts.
func (s *Service) Delete(ctx context.Context, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}
	if err := s.store.DeleteRolloutsBySpec(ctx, id); err != nil {
		return err
	}
	return s.store.DeleteSpec(ctx, id)
}

// RolloutsBySpec returns all rollout records associated with a spec.
func (s *Service) RolloutsBySpec(ctx context.Context, specID string, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	if specID == "" {
		return nil, storage.ErrInvalidArgument
	}

	res, err := s.store.ListRollouts(ctx, filter, storage.ListOptions{Limit: storage.MaxListLimit})
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

// Deploy initiates distribution of a spec to all its target agents.
//
// For each agent in [model.Spec.Targets] the method either updates an existing rollout record or creates a new one,
// setting status to pending with the current spec version.
//
// The sync runner will later pick up pending rollouts and push the spec payload to the agents.
func (s *Service) Deploy(ctx context.Context, specID string) error {
	ts, err := s.store.GetSpec(ctx, specID)
	if err != nil {
		return err
	}

	var existing *model.Rollout
	for _, agentID := range ts.Targets() {
		existing, err = s.store.GetRollout(ctx, model.RolloutID(specID, agentID))
		if err == nil {
			existing.MarkPending(ts.Version())
			if err = s.store.UpsertRollout(ctx, existing); err != nil {
				return err
			}
			continue
		}

		var rollout *model.Rollout
		rollout, err = model.NewRollout(specID, agentID, ts.Version())
		if err != nil {
			return err
		}
		if err = s.store.UpsertRollout(ctx, rollout); err != nil {
			return err
		}
	}
	return nil
}
