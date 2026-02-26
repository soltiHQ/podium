// Package agent implements agent management use-cases:
//   - Paginated listing and retrieval
//   - Upsert with label and heartbeat preservation
//   - Control-plane label patching.
package agent

import (
	"context"
	"errors"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Service provides agent management operations.
type Service struct {
	store storage.AgentStore
}

// New creates a new agent service.
func New(store storage.AgentStore) *Service {
	if store == nil {
		panic("agent.Service: store is nil")
	}
	return &Service{store: store}
}

// List returns a page of agents matching the query.
func (s *Service) List(ctx context.Context, q ListQuery) (*Page, error) {
	res, err := s.store.ListAgents(ctx, q.Filter, storage.ListOptions{
		Limit:  service.NormalizeListLimit(q.Limit, defaultListLimit),
		Cursor: q.Cursor,
	})
	if err != nil {
		return nil, err
	}

	out := make([]*model.Agent, 0, len(res.Items))
	for _, a := range res.Items {
		if a == nil {
			continue
		}
		out = append(out, a.Clone())
	}
	return &Page{
		Items:      out,
		NextCursor: res.NextCursor,
	}, nil
}

// Get returns a single agent by ID.
func (s *Service) Get(ctx context.Context, id string) (*model.Agent, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}

	a, err := s.store.GetAgent(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, storage.ErrInternal
	}
	return a.Clone(), nil
}

// Upsert an agent.
//
// If the agent already exists, control-plane owned labels and the original
// createdAt timestamp are preserved because they are not part of the
// discovery payload reported by the agent.
func (s *Service) Upsert(ctx context.Context, m *model.Agent) error {
	existing, err := s.store.GetAgent(ctx, m.ID())
	switch {
	case err == nil:
		m.SetCreatedAt(existing.CreatedAt())
		for k, v := range existing.LabelsAll() {
			m.LabelAdd(k, v)
		}
		if m.HeartbeatInterval() == 0 && existing.HeartbeatInterval() > 0 {
			m.SetHeartbeatInterval(existing.HeartbeatInterval())
		}
	case errors.Is(err, storage.ErrNotFound):
	default:
		return err
	}
	return s.store.UpsertAgent(ctx, m)
}

// PatchLabels replaces labels for an agent.
func (s *Service) PatchLabels(ctx context.Context, req PatchLabels) (*model.Agent, error) {
	if req.ID == "" {
		return nil, storage.ErrInvalidArgument
	}

	agent, err := s.store.GetAgent(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, storage.ErrInternal
	}

	replaceLabels(agent, req.Labels)
	if err = s.store.UpsertAgent(ctx, agent); err != nil {
		return nil, err
	}
	return agent.Clone(), nil
}

func replaceLabels(a *model.Agent, labels map[string]string) {
	for k := range a.LabelsAll() {
		a.LabelDelete(k)
	}
	for k, v := range labels {
		if k == "" || v == "" {
			continue
		}
		a.LabelAdd(k, v)
	}
}
