package raft

import (
	"context"
	"fmt"
	"sync"
	"time"

	hraft "github.com/hashicorp/raft"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
)

// Compile-time check.
var (
	_ storage.Storage                = (*Store)(nil)
	_ storage.ClockedSessionRotation = (*Store)(nil)
)

// applyTimeout bounds the wait for a Raft log entry to be committed.
const applyTimeout = 5 * time.Second

// Store wraps an inner storage.Storage. Reads go local (eventual
// consistency). Writes are encoded as protobuf Commands and submitted to
// raft.Apply; the FSM applies them to the inner store on every replica
// in log order.
//
// Write flow:
//
//   - WithTx runs fn on an overlay.Tx over the inner store, which records
//     each write as an op, then submits the ops as one multi-op Command for
//     atomic replication. fn reads its own writes everywhere: point reads,
//     index lookups, listings and scans.
//   - Every single mutation method is a WithTx of that one write, as in the
//     durable store. The overlay checks the write as the FSM will check the
//     op when it applies it, before anything is proposed: the delete of an
//     absent row gives ErrNotFound to its caller and no command is proposed.
//     So a command that fails to apply is a corrupted log entry or a version
//     skew.
//
// The inner store must be the plain inmemory one and MUST be the same store
// the FSM applies against. Otherwise leader and followers diverge.
type Store struct {
	inner storage.Storage
	raft  *hraft.Raft

	txMu sync.Mutex // single in-flight WithTx at a time
}

// NewStore builds a Raft-backed Store.
func NewStore(inner storage.Storage, r *hraft.Raft) *Store {
	if inner == nil {
		panic("raft: nil inner store")
	}
	if r == nil {
		panic("raft: nil raft")
	}
	return &Store{inner: inner, raft: r}
}

// apply submits a Command to Raft, waiting up to applyTimeout for commit. Its
// errors from raft are given their meaning by clusterError. Its caller holds txMu.
func (s *Store) apply(ops []*raftv1.Op) error {
	data, err := overlay.EncodeCommand(ops)
	if err != nil {
		return err
	}
	f := s.raft.Apply(data, applyTimeout)
	if err := f.Error(); err != nil {
		return fmt.Errorf("raft: apply: %w", clusterError(err))
	}
	if resp := f.Response(); resp != nil {
		if e, ok := resp.(error); ok {
			return e
		}
	}
	return nil
}

// === Agents ===

func (s *Store) UpsertAgent(ctx context.Context, a *model.Agent) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertAgent(ctx, a) })
}

func (s *Store) GetAgent(ctx context.Context, id string) (*model.Agent, error) {
	return s.inner.GetAgent(ctx, id)
}

func (s *Store) ListAgents(ctx context.Context, f storage.AgentFilter, o storage.ListOptions) (*storage.AgentListResult, error) {
	return s.inner.ListAgents(ctx, f, o)
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteAgent(ctx, id) })
}

// === Agent credentials ===

func (s *Store) UpsertAgentCredential(ctx context.Context, c *model.AgentCredential) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertAgentCredential(ctx, c) })
}

func (s *Store) GetAgentCredential(ctx context.Context, agentID string) (*model.AgentCredential, error) {
	return s.inner.GetAgentCredential(ctx, agentID)
}

func (s *Store) DeleteAgentCredential(ctx context.Context, agentID string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteAgentCredential(ctx, agentID) })
}

// === Users ===

func (s *Store) UpsertUser(ctx context.Context, u *model.User) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertUser(ctx, u) })
}

func (s *Store) GetUser(ctx context.Context, id string) (*model.User, error) {
	return s.inner.GetUser(ctx, id)
}

func (s *Store) GetUserBySubject(ctx context.Context, sub string) (*model.User, error) {
	return s.inner.GetUserBySubject(ctx, sub)
}

func (s *Store) ListUsers(ctx context.Context, f storage.UserFilter, o storage.ListOptions) (*storage.UserListResult, error) {
	return s.inner.ListUsers(ctx, f, o)
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteUser(ctx, id) })
}

// === Roles ===

func (s *Store) UpsertRole(ctx context.Context, r *model.Role) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertRole(ctx, r) })
}

func (s *Store) GetRole(ctx context.Context, id string) (*model.Role, error) {
	return s.inner.GetRole(ctx, id)
}

func (s *Store) GetRoles(ctx context.Context, ids []string) ([]*model.Role, error) {
	return s.inner.GetRoles(ctx, ids)
}

func (s *Store) GetRoleByName(ctx context.Context, name string) (*model.Role, error) {
	return s.inner.GetRoleByName(ctx, name)
}

func (s *Store) ListRoles(ctx context.Context, f storage.RoleFilter, o storage.ListOptions) (*storage.RoleListResult, error) {
	return s.inner.ListRoles(ctx, f, o)
}

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteRole(ctx, id) })
}

// === Credentials ===

func (s *Store) UpsertCredential(ctx context.Context, c *model.Credential) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertCredential(ctx, c) })
}

func (s *Store) GetCredential(ctx context.Context, id string) (*model.Credential, error) {
	return s.inner.GetCredential(ctx, id)
}

func (s *Store) GetCredentialByUserAndAuth(ctx context.Context, userID string, auth enum.Auth) (*model.Credential, error) {
	return s.inner.GetCredentialByUserAndAuth(ctx, userID, auth)
}

func (s *Store) ListCredentialsByUser(ctx context.Context, userID string) ([]*model.Credential, error) {
	return s.inner.ListCredentialsByUser(ctx, userID)
}

func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteCredential(ctx, id) })
}

// === Verifiers ===

func (s *Store) UpsertVerifier(ctx context.Context, v *model.Verifier) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertVerifier(ctx, v) })
}

func (s *Store) GetVerifier(ctx context.Context, id string) (*model.Verifier, error) {
	return s.inner.GetVerifier(ctx, id)
}

func (s *Store) GetVerifierByCredential(ctx context.Context, credID string) (*model.Verifier, error) {
	return s.inner.GetVerifierByCredential(ctx, credID)
}

func (s *Store) DeleteVerifier(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteVerifier(ctx, id) })
}

func (s *Store) DeleteVerifierByCredential(ctx context.Context, credID string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteVerifierByCredential(ctx, credID) })
}

// === Sessions ===

func (s *Store) CreateSession(ctx context.Context, ss *model.Session) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.CreateSession(ctx, ss) })
}

func (s *Store) GetSession(ctx context.Context, id string) (*model.Session, error) {
	return s.inner.GetSession(ctx, id)
}

func (s *Store) ListSessionsByUser(ctx context.Context, userID string) ([]*model.Session, error) {
	return s.inner.ListSessionsByUser(ctx, userID)
}

func (s *Store) RotateRefresh(ctx context.Context, sessionID string, newHash []byte, newExpiresAt time.Time) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.RotateRefresh(ctx, sessionID, newHash, newExpiresAt) })
}

// RotateRefreshAt is RotateRefresh with the modification time its caller chose,
// which the command carries to every replica (see storage.ClockedSessionRotation).
func (s *Store) RotateRefreshAt(ctx context.Context, sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) error {
	return s.WithTx(ctx, func(tx storage.Storage) error {
		clocked, ok := tx.(storage.ClockedSessionRotation)
		if !ok {
			return fmt.Errorf("%w: explicit session rotation timestamps are unsupported", storage.ErrInternal)
		}
		return clocked.RotateRefreshAt(ctx, sessionID, newHash, newExpiresAt, updatedAt)
	})
}

func (s *Store) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.RevokeSession(ctx, sessionID, revokedAt) })
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteSession(ctx, id) })
}

func (s *Store) DeleteSessionsByUser(ctx context.Context, userID string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteSessionsByUser(ctx, userID) })
}

// === Specs ===

func (s *Store) UpsertSpec(ctx context.Context, ts *model.Spec) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertSpec(ctx, ts) })
}

func (s *Store) GetSpec(ctx context.Context, id string) (*model.Spec, error) {
	return s.inner.GetSpec(ctx, id)
}

func (s *Store) ListSpecs(ctx context.Context, f storage.SpecFilter, o storage.ListOptions) (*storage.SpecListResult, error) {
	return s.inner.ListSpecs(ctx, f, o)
}

func (s *Store) DeleteSpec(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteSpec(ctx, id) })
}

// === Rollouts ===

func (s *Store) UpsertRollout(ctx context.Context, r *model.Rollout) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertRollout(ctx, r) })
}

func (s *Store) GetRollout(ctx context.Context, id string) (*model.Rollout, error) {
	return s.inner.GetRollout(ctx, id)
}

func (s *Store) ListRollouts(ctx context.Context, f storage.RolloutFilter, o storage.ListOptions) (*storage.RolloutListResult, error) {
	return s.inner.ListRollouts(ctx, f, o)
}

func (s *Store) DeleteRollout(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteRollout(ctx, id) })
}

func (s *Store) DeleteRolloutsBySpec(ctx context.Context, specID string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteRolloutsBySpec(ctx, specID) })
}

// === FilterFactory — passthrough ===

func (s *Store) BuildRolloutFilter(c storage.RolloutQueryCriteria) storage.RolloutFilter {
	return s.inner.BuildRolloutFilter(c)
}

func (s *Store) BuildSpecFilter(c storage.SpecQueryCriteria) storage.SpecFilter {
	return s.inner.BuildSpecFilter(c)
}

// === WithTx — recording batch ===

// WithTx runs fn on an overlay.Tx that records every mutation as a proto Op.
// At end of fn the recorded ops are submitted to Raft as one Command and
// applied atomically on every replica; a transaction that recorded none
// submits nothing.
//
// fn's reads within the tx see its own writes over the inner store. While this
// node leads, the inner store does not move under fn: every write of this Store
// holds txMu, and fn runs after the FSM has applied every committed command. A
// node that loses leadership while fn runs fails the submission.
//
// Contract: only leader can run WithTx. If Raft.State() != Leader the tx is
// rejected with ErrNotLeader before fn executes.
func (s *Store) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.raft.State() != hraft.Leader {
		return fmt.Errorf("raft: WithTx: %w", ErrNotLeader)
	}

	s.txMu.Lock()
	defer s.txMu.Unlock()

	// A newly elected leader may still be replaying committed commands. Never
	// evaluate a transaction against the temporarily empty/stale inner store.
	if err := awaitApplied(ctx, s.raft); err != nil {
		return err
	}
	tx := overlay.New(s.inner)
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tx.Empty() {
		return nil
	}
	return s.apply(tx.Ops())
}
