package durable

import (
	"context"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func (d *Store) UpsertAgent(ctx context.Context, a *model.Agent) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertAgent(ctx, a) })
}

func (d *Store) GetAgent(ctx context.Context, id string) (*model.Agent, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Agent, error) { return inner.GetAgent(ctx, id) })
}

func (d *Store) ListAgents(ctx context.Context, filter storage.AgentFilter, opts storage.ListOptions) (*storage.AgentListResult, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*storage.AgentListResult, error) {
		return inner.ListAgents(ctx, filter, opts)
	})
}

func (d *Store) DeleteAgent(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteAgent(ctx, id) })
}

func (d *Store) UpsertAgentCredential(ctx context.Context, c *model.AgentCredential) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertAgentCredential(ctx, c) })
}

func (d *Store) GetAgentCredential(ctx context.Context, agentID string) (*model.AgentCredential, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.AgentCredential, error) {
		return inner.GetAgentCredential(ctx, agentID)
	})
}

func (d *Store) DeleteAgentCredential(ctx context.Context, agentID string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteAgentCredential(ctx, agentID) })
}

func (d *Store) UpsertUser(ctx context.Context, u *model.User) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertUser(ctx, u) })
}

func (d *Store) GetUser(ctx context.Context, id string) (*model.User, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.User, error) { return inner.GetUser(ctx, id) })
}

func (d *Store) GetUserBySubject(ctx context.Context, subject string) (*model.User, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.User, error) { return inner.GetUserBySubject(ctx, subject) })
}

func (d *Store) ListUsers(ctx context.Context, filter storage.UserFilter, opts storage.ListOptions) (*storage.UserListResult, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*storage.UserListResult, error) {
		return inner.ListUsers(ctx, filter, opts)
	})
}

func (d *Store) DeleteUser(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteUser(ctx, id) })
}

func (d *Store) UpsertCredential(ctx context.Context, c *model.Credential) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertCredential(ctx, c) })
}

func (d *Store) GetCredential(ctx context.Context, id string) (*model.Credential, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Credential, error) { return inner.GetCredential(ctx, id) })
}

func (d *Store) GetCredentialByUserAndAuth(ctx context.Context, userID string, auth enum.Auth) (*model.Credential, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Credential, error) {
		return inner.GetCredentialByUserAndAuth(ctx, userID, auth)
	})
}

func (d *Store) ListCredentialsByUser(ctx context.Context, userID string) ([]*model.Credential, error) {
	return readState(ctx, d, func(inner *inmemory.Store) ([]*model.Credential, error) {
		return inner.ListCredentialsByUser(ctx, userID)
	})
}

func (d *Store) DeleteCredential(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteCredential(ctx, id) })
}

func (d *Store) UpsertVerifier(ctx context.Context, v *model.Verifier) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertVerifier(ctx, v) })
}

func (d *Store) GetVerifier(ctx context.Context, id string) (*model.Verifier, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Verifier, error) { return inner.GetVerifier(ctx, id) })
}

func (d *Store) GetVerifierByCredential(ctx context.Context, credentialID string) (*model.Verifier, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Verifier, error) {
		return inner.GetVerifierByCredential(ctx, credentialID)
	})
}

func (d *Store) DeleteVerifierByCredential(ctx context.Context, credentialID string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteVerifierByCredential(ctx, credentialID) })
}

func (d *Store) DeleteVerifier(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteVerifier(ctx, id) })
}

func (d *Store) CreateSession(ctx context.Context, s *model.Session) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.CreateSession(ctx, s) })
}

func (d *Store) GetSession(ctx context.Context, id string) (*model.Session, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Session, error) { return inner.GetSession(ctx, id) })
}

func (d *Store) ListSessionsByUser(ctx context.Context, userID string) ([]*model.Session, error) {
	return readState(ctx, d, func(inner *inmemory.Store) ([]*model.Session, error) { return inner.ListSessionsByUser(ctx, userID) })
}

func (d *Store) RotateRefresh(ctx context.Context, sessionID string, newHash []byte, newExpiresAt time.Time) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.RotateRefresh(ctx, sessionID, newHash, newExpiresAt) })
}

func (d *Store) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.RevokeSession(ctx, sessionID, revokedAt) })
}

func (d *Store) DeleteSession(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteSession(ctx, id) })
}

func (d *Store) DeleteSessionsByUser(ctx context.Context, userID string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteSessionsByUser(ctx, userID) })
}

func (d *Store) UpsertRole(ctx context.Context, r *model.Role) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertRole(ctx, r) })
}

func (d *Store) GetRole(ctx context.Context, id string) (*model.Role, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Role, error) { return inner.GetRole(ctx, id) })
}

func (d *Store) GetRoles(ctx context.Context, ids []string) ([]*model.Role, error) {
	return readState(ctx, d, func(inner *inmemory.Store) ([]*model.Role, error) { return inner.GetRoles(ctx, ids) })
}

func (d *Store) GetRoleByName(ctx context.Context, name string) (*model.Role, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Role, error) { return inner.GetRoleByName(ctx, name) })
}

func (d *Store) ListRoles(ctx context.Context, filter storage.RoleFilter, opts storage.ListOptions) (*storage.RoleListResult, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*storage.RoleListResult, error) {
		return inner.ListRoles(ctx, filter, opts)
	})
}

func (d *Store) DeleteRole(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteRole(ctx, id) })
}

func (d *Store) UpsertSpec(ctx context.Context, ts *model.Spec) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertSpec(ctx, ts) })
}

func (d *Store) GetSpec(ctx context.Context, id string) (*model.Spec, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Spec, error) { return inner.GetSpec(ctx, id) })
}

func (d *Store) ListSpecs(ctx context.Context, filter storage.SpecFilter, opts storage.ListOptions) (*storage.SpecListResult, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*storage.SpecListResult, error) {
		return inner.ListSpecs(ctx, filter, opts)
	})
}

func (d *Store) DeleteSpec(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteSpec(ctx, id) })
}

func (d *Store) UpsertRollout(ctx context.Context, ss *model.Rollout) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertRollout(ctx, ss) })
}

func (d *Store) GetRollout(ctx context.Context, id string) (*model.Rollout, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.Rollout, error) { return inner.GetRollout(ctx, id) })
}

func (d *Store) ListRollouts(ctx context.Context, filter storage.RolloutFilter, opts storage.ListOptions) (*storage.RolloutListResult, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*storage.RolloutListResult, error) {
		return inner.ListRollouts(ctx, filter, opts)
	})
}

func (d *Store) DeleteRollout(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteRollout(ctx, id) })
}

func (d *Store) DeleteRolloutsBySpec(ctx context.Context, specID string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteRolloutsBySpec(ctx, specID) })
}

func (d *Store) BuildRolloutFilter(c storage.RolloutQueryCriteria) storage.RolloutFilter {
	return d.filters.BuildRolloutFilter(c)
}

func (d *Store) BuildSpecFilter(c storage.SpecQueryCriteria) storage.SpecFilter {
	return d.filters.BuildSpecFilter(c)
}
