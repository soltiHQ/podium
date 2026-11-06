package overlay

import (
	"context"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// An upsert keeps the row its operation decodes to, which is the row the state
// machine stores, so a read in the transaction returns what a commit stores.

func (t *Tx) UpsertAgent(_ context.Context, a *model.Agent) error {
	if a == nil {
		return storage.ErrInvalidArgument
	}
	msg := wire.AgentToProto(a)
	row, err := wire.AgentFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.agents.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_AgentUpsert{AgentUpsert: msg}})
	return nil
}

func (t *Tx) GetAgent(ctx context.Context, id string) (*model.Agent, error) {
	return t.agents.get(ctx, id, t.base.GetAgent)
}

func (t *Tx) ListAgents(ctx context.Context, filter storage.AgentFilter, opts storage.ListOptions) (*storage.AgentListResult, error) {
	return list(opts, func() ([]*model.Agent, error) { return t.ScanAgents(ctx, filter) })
}

func (t *Tx) ScanAgents(ctx context.Context, filter storage.AgentFilter) ([]*model.Agent, error) {
	return scan(&t.agents, filter, func() ([]*model.Agent, error) { return storage.AllAgents(ctx, t.base, filter) })
}

func (t *Tx) DeleteAgent(ctx context.Context, id string) error {
	if err := t.agents.remove(ctx, id, t.base.GetAgent); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_AgentDelete{AgentDelete: id}})
	return nil
}

func (t *Tx) UpsertAgentCredential(_ context.Context, c *model.AgentCredential) error {
	if c == nil {
		return storage.ErrInvalidArgument
	}
	msg := wire.AgentCredentialToProto(c)
	row, err := wire.AgentCredentialFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.agentCreds.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_AgentCredentialUpsert{AgentCredentialUpsert: msg}})
	return nil
}

func (t *Tx) GetAgentCredential(ctx context.Context, agentID string) (*model.AgentCredential, error) {
	return t.agentCreds.get(ctx, agentID, t.base.GetAgentCredential)
}

func (t *Tx) ListAgentCredentials(ctx context.Context, opts storage.ListOptions) (*storage.AgentCredentialListResult, error) {
	return list(opts, func() ([]*model.AgentCredential, error) {
		return scan(&t.agentCreds, nil, func() ([]*model.AgentCredential, error) {
			return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.AgentCredentialListResult, error) {
				return t.base.ListAgentCredentials(ctx, o)
			})
		})
	})
}

func (t *Tx) DeleteAgentCredential(ctx context.Context, agentID string) error {
	if err := t.agentCreds.remove(ctx, agentID, t.base.GetAgentCredential); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_AgentCredentialDelete{AgentCredentialDelete: agentID}})
	return nil
}
