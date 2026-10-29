package inmemory

import (
	"context"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Public operations share the store-wide gate with transactions.
func (s *Store) UpsertAgent(ctx context.Context, a *model.Agent) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertAgent(ctx, a)
}

func (s *Store) GetAgent(ctx context.Context, id string) (*model.Agent, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetAgent(ctx, id)
}

func (s *Store) ListAgents(ctx context.Context, filter storage.AgentFilter, opts storage.ListOptions) (*storage.AgentListResult, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListAgents(ctx, filter, opts)
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteAgent(ctx, id)
}

func (s *Store) UpsertAgentCredential(ctx context.Context, c *model.AgentCredential) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertAgentCredential(ctx, c)
}

func (s *Store) GetAgentCredential(ctx context.Context, agentID string) (*model.AgentCredential, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetAgentCredential(ctx, agentID)
}

func (s *Store) DeleteAgentCredential(ctx context.Context, agentID string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteAgentCredential(ctx, agentID)
}

func (s *Store) UpsertUser(ctx context.Context, u *model.User) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertUser(ctx, u)
}

func (s *Store) GetUser(ctx context.Context, id string) (*model.User, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetUser(ctx, id)
}

func (s *Store) GetUserBySubject(ctx context.Context, subject string) (*model.User, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetUserBySubject(ctx, subject)
}

func (s *Store) ListUsers(ctx context.Context, filter storage.UserFilter, opts storage.ListOptions) (*storage.UserListResult, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListUsers(ctx, filter, opts)
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteUser(ctx, id)
}

func (s *Store) UpsertCredential(ctx context.Context, c *model.Credential) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertCredential(ctx, c)
}

func (s *Store) GetCredential(ctx context.Context, id string) (*model.Credential, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetCredential(ctx, id)
}

func (s *Store) GetCredentialByUserAndAuth(ctx context.Context, userID string, auth enum.Auth) (*model.Credential, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetCredentialByUserAndAuth(ctx, userID, auth)
}

func (s *Store) ListCredentialsByUser(ctx context.Context, userID string) ([]*model.Credential, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListCredentialsByUser(ctx, userID)
}

func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteCredential(ctx, id)
}

func (s *Store) UpsertVerifier(ctx context.Context, v *model.Verifier) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertVerifier(ctx, v)
}

func (s *Store) GetVerifier(ctx context.Context, id string) (*model.Verifier, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetVerifier(ctx, id)
}

func (s *Store) GetVerifierByCredential(ctx context.Context, credentialID string) (*model.Verifier, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetVerifierByCredential(ctx, credentialID)
}

func (s *Store) DeleteVerifier(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteVerifier(ctx, id)
}

func (s *Store) DeleteVerifierByCredential(ctx context.Context, credentialID string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteVerifierByCredential(ctx, credentialID)
}

func (s *Store) CreateSession(ctx context.Context, sess *model.Session) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.CreateSession(ctx, sess)
}

func (s *Store) GetSession(ctx context.Context, id string) (*model.Session, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetSession(ctx, id)
}

func (s *Store) ListSessionsByUser(ctx context.Context, userID string) ([]*model.Session, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListSessionsByUser(ctx, userID)
}

func (s *Store) RotateRefresh(ctx context.Context, sessionID string, newHash []byte, newExpiresAt time.Time) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.RotateRefresh(ctx, sessionID, newHash, newExpiresAt)
}

func (s *Store) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.RevokeSession(ctx, sessionID, revokedAt)
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteSession(ctx, id)
}

func (s *Store) DeleteSessionsByUser(ctx context.Context, userID string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteSessionsByUser(ctx, userID)
}

func (s *Store) UpsertRole(ctx context.Context, r *model.Role) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertRole(ctx, r)
}

func (s *Store) GetRole(ctx context.Context, id string) (*model.Role, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetRole(ctx, id)
}

func (s *Store) GetRoles(ctx context.Context, ids []string) ([]*model.Role, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetRoles(ctx, ids)
}

func (s *Store) GetRoleByName(ctx context.Context, name string) (*model.Role, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetRoleByName(ctx, name)
}

func (s *Store) ListRoles(ctx context.Context, filter storage.RoleFilter, opts storage.ListOptions) (*storage.RoleListResult, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListRoles(ctx, filter, opts)
}

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteRole(ctx, id)
}

func (s *Store) UpsertSpec(ctx context.Context, ts *model.Spec) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertSpec(ctx, ts)
}

func (s *Store) GetSpec(ctx context.Context, id string) (*model.Spec, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetSpec(ctx, id)
}

func (s *Store) ListSpecs(ctx context.Context, filter storage.SpecFilter, opts storage.ListOptions) (*storage.SpecListResult, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListSpecs(ctx, filter, opts)
}

func (s *Store) DeleteSpec(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteSpec(ctx, id)
}

func (s *Store) UpsertRollout(ctx context.Context, ss *model.Rollout) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertRollout(ctx, ss)
}

func (s *Store) GetRollout(ctx context.Context, id string) (*model.Rollout, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetRollout(ctx, id)
}

func (s *Store) ListRollouts(ctx context.Context, filter storage.RolloutFilter, opts storage.ListOptions) (*storage.RolloutListResult, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListRollouts(ctx, filter, opts)
}

func (s *Store) DeleteRollout(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteRollout(ctx, id)
}

func (s *Store) DeleteRolloutsBySpec(ctx context.Context, specID string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteRolloutsBySpec(ctx, specID)
}
