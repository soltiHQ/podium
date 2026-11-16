package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// An agent's row holds its status and staleness deadline beside the body, for
// the criteria and the counts: stale_at is 0 for a deadline never set, which the
// codec's refusal of the exact Unix epoch keeps unambiguous.

var (
	agentKind   = kind[*model.Agent]{"podium_agents", snapshot.UnmarshalAgentJSON}
	upsertAgent = upsertSQL("podium_agents", "created_at", "status", "stale_at", "body")
)

// UpsertAgent creates or replaces an agent.
func (w *writer) UpsertAgent(ctx context.Context, a *model.Agent) error {
	if err := storage.ValidateEntity(a); err != nil {
		return err
	}
	body, err := snapshot.MarshalAgentJSON(a)
	if err != nil {
		return err
	}
	id, err := indexKey("agent ID", a.ID())
	if err != nil {
		return err
	}
	created, err := columnTime("creation time", a.CreatedAt())
	if err != nil {
		return err
	}
	stale, err := columnTime("staleness deadline", a.StaleAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, upsertAgent, id, created, int16(a.Status()), stale, body)
	return err
}

// GetAgent returns the agent with the ID.
func (r *reader) GetAgent(ctx context.Context, id string) (*model.Agent, error) {
	return byID(ctx, r, agentKind, id)
}

// ListAgents returns a page of the agents the filter matches.
func (r *reader) ListAgents(ctx context.Context, filter storage.AgentFilter, opts storage.ListOptions) (*storage.AgentListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, err := agentQuery(filter)
	if err != nil {
		return nil, err
	}
	return listPage(ctx, r, q, opts)
}

// DeleteAgent deletes the agent with the ID.
func (w *writer) DeleteAgent(ctx context.Context, id string) error {
	return w.deleteByID(ctx, agentKind.table, id)
}

// UpsertAgent creates or replaces an agent, in a write transaction of its own.
func (s *Store) UpsertAgent(ctx context.Context, a *model.Agent) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertAgent(ctx, a) })
}

// ListAgents returns a page of the agents the filter matches, from the replica
// for a read-only context.
func (s *Store) ListAgents(ctx context.Context, filter storage.AgentFilter, opts storage.ListOptions) (*storage.AgentListResult, error) {
	return routed(ctx, s, func(r *reader) (*storage.AgentListResult, error) { return r.ListAgents(ctx, filter, opts) })
}

// DeleteAgent deletes the agent with the ID, in a write transaction of its own.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteAgent(ctx, id) })
}
