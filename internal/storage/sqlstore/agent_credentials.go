package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// An agent credential is keyed by its agent's ID: one row per agent.

var (
	agentCredentialKind   = kind[*model.AgentCredential]{"podium_agent_credentials", snapshot.UnmarshalAgentCredentialJSON}
	upsertAgentCredential = upsertSQL("podium_agent_credentials", "created_at", "body")
)

// UpsertAgentCredential stores or replaces an agent's credential.
func (w *writer) UpsertAgentCredential(ctx context.Context, c *model.AgentCredential) error {
	if err := storage.ValidateEntity(c); err != nil {
		return err
	}
	body, err := snapshot.MarshalAgentCredentialJSON(c)
	if err != nil {
		return err
	}
	id, err := indexKey("agent ID", c.ID())
	if err != nil {
		return err
	}
	created, err := columnTime("creation time", c.CreatedAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, upsertAgentCredential, id, created, body)
	return err
}

// GetAgentCredential returns the credential of the agent.
func (r *reader) GetAgentCredential(ctx context.Context, agentID string) (*model.AgentCredential, error) {
	return byID(ctx, r, agentCredentialKind, agentID)
}

// ListAgentCredentials returns a page of the agent credentials.
func (r *reader) ListAgentCredentials(ctx context.Context, opts storage.ListOptions) (*storage.AgentCredentialListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return listPage(ctx, r, pageQuery[*model.AgentCredential]{kind: agentCredentialKind, where: &where{}}, opts)
}

// DeleteAgentCredential deletes the credential of the agent.
func (w *writer) DeleteAgentCredential(ctx context.Context, agentID string) error {
	return w.deleteByID(ctx, agentCredentialKind.table, agentID)
}

// UpsertAgentCredential stores or replaces an agent's credential, in a write
// transaction of its own.
func (s *Store) UpsertAgentCredential(ctx context.Context, c *model.AgentCredential) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertAgentCredential(ctx, c) })
}

// ListAgentCredentials returns a page of the agent credentials, from the replica
// for a read-only context.
func (s *Store) ListAgentCredentials(ctx context.Context, opts storage.ListOptions) (*storage.AgentCredentialListResult, error) {
	return routed(ctx, s, func(r *reader) (*storage.AgentCredentialListResult, error) {
		return r.ListAgentCredentials(ctx, opts)
	})
}

// DeleteAgentCredential deletes the credential of the agent, in a write
// transaction of its own.
func (s *Store) DeleteAgentCredential(ctx context.Context, agentID string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteAgentCredential(ctx, agentID) })
}
