package agent

import (
	"context"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/storage"
)

type Service struct {
	logger zerolog.Logger
	store  storage.AgentStore
}

func New(store storage.AgentStore, logger zerolog.Logger) *Service {
	if store == nil {
		panic("agent.Service: store is nil")
	}
	return &Service{
		logger: logger.With().Str("service", "agents").Logger(),
		store:  store,
	}
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
	existing, _ := s.store.GetAgent(ctx, m.ID())
	if existing != nil {
		m.SetCreatedAt(existing.CreatedAt())
		for k, v := range existing.LabelsAll() {
			m.LabelAdd(k, v)
		}
	}
	return s.store.UpsertAgent(ctx, m)
}

// PatchLabels replaces labels for an agent (control-plane owned).
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
