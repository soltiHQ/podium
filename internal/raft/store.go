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
//   - The first transaction of each leadership term of this node starts with
//     a barrier, so that it reads every write committed before it; the later
//     ones of the term start with none (awaitReady), and those that propose
//     nothing ask a quorum whether this node still leads the term they read
//     in before they answer, which writes nothing, once they released txMu
//     (answer). A heartbeat, a token check that writes nothing and an upsert,
//     so commits one entry.
//
// The inner store must be the plain inmemory one and MUST be the same store
// the FSM applies against. Otherwise leader and followers diverge.
type Store struct {
	inner storage.Storage
	raft  *hraft.Raft
	// propose submits a command to raft: raft.Apply, which tests replace.
	propose func(cmd []byte, timeout time.Duration) hraft.ApplyFuture
	// barrier commits a barrier and waits until the FSM has applied every entry
	// before it: awaitApplied, which tests replace.
	barrier func(ctx context.Context) error
	// verifyLeader asks a quorum whether this node still leads, and writes
	// nothing: the error of raft.VerifyLeader, which tests replace. answer
	// waits for it as awaitLeadership does.
	verifyLeader func() error
	// stopped is closed once raft shuts down, which ends the wait for a write
	// (awaitWrite).
	stopped <-chan struct{}

	txMu sync.Mutex // single in-flight WithTx at a time
	// readyTerm is the term of the last barrier that succeeded while this node
	// led, 0 before the first; txMu guards it (see awaitReady).
	readyTerm uint64
}

// NewStore builds a Raft-backed Store.
func NewStore(inner storage.Storage, r *hraft.Raft) *Store {
	if inner == nil {
		panic("raft: nil inner store")
	}
	if r == nil {
		panic("raft: nil raft")
	}
	return &Store{
		inner:        inner,
		raft:         r,
		propose:      r.Apply,
		barrier:      func(ctx context.Context) error { return awaitApplied(ctx, r) },
		verifyLeader: func() error { return r.VerifyLeader().Error() },
		stopped:      stopSignal(r),
	}
}

// apply submits a Command to Raft, and waits for its commit until raft
// answers, or shuts down (awaitWrite); applyTimeout bounds only the enqueueing
// of the command. Its errors from raft are given their meaning by writeError:
// a write of unknown outcome, which may have committed, is
// cluster.ErrLeadershipLost. A node that shut down already proposes nothing:
// storage.ErrUnavailable (refuseStopped). Its caller holds txMu, which it
// releases once apply returns.
func (s *Store) apply(ops []*raftv1.Op) error {
	data, err := overlay.EncodeCommand(ops)
	if err != nil {
		return err
	}
	if err := refuseStopped(s.stopped); err != nil {
		return fmt.Errorf("raft: apply: %w", err)
	}
	f := s.propose(data, applyTimeout)
	if err := awaitWrite(f, s.stopped); err != nil {
		return fmt.Errorf("raft: apply: %w", writeError(f, err))
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
// fn's reads within the tx see its own writes over the inner store, which holds
// every write committed before the transaction began, unless this node is a
// deposed leader that has not heard of the later term yet: a newly elected
// leader may still be replaying committed commands, so the first transaction of
// each leadership term of this node waits for a barrier, and every later one of
// the term reads what the earlier ones applied (awaitReady). While this node
// leads, the inner store does not move under fn: every write of this Store
// holds txMu. A write whose term ended before it was proposed fails with
// ErrNotLeader, having proposed nothing. A later transaction of the term that
// proposes nothing answers once a quorum confirmed that this node still leads
// the term fn read in (answer), so that a deposed leader does not answer from
// its stale state; the check runs after txMu is released.
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
	term, check, verdict := s.transact(ctx, fn)
	if !check {
		return verdict
	}
	return s.answer(ctx, term, verdict)
}

// transact runs the transaction of WithTx under txMu: it waits until the term
// is ready (awaitReady), runs fn on an overlay of the inner store, and proposes
// what fn wrote. It returns the term fn read in, and the transaction's error,
// and whether that verdict still needs answer's check of the leadership: one of
// a transaction that skipped the barrier of its term and proposes nothing. Any
// other verdict is final.
func (s *Store) transact(ctx context.Context, fn func(tx storage.Storage) error) (term uint64, check bool, verdict error) {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	term, skipped, err := s.awaitReady(ctx)
	if err != nil {
		return 0, false, err
	}
	tx := overlay.New(s.inner)
	if err := fn(tx); err != nil {
		return term, skipped, err
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if tx.Empty() {
		return term, skipped, nil
	}
	// fn read the state of term. In a later term this node may have lost the
	// leadership, and other leaders committed writes that fn did not read, and
	// the inner store may have moved under fn as it applied them. So the term is
	// read again right before the write is proposed, and a term that changed
	// refuses the write: nothing was proposed, which is ErrNotLeader, as for a
	// barrier that lost the leadership (barrierError). A window remains from
	// this check to raft taking the command, in which a node that lost and
	// regained the leadership would propose in the new term what fn read in the
	// old one. It is narrower than the window from the barrier to the apply that
	// every transaction had when each took a barrier, which spanned fn too. The
	// FSM checks no term: during a rolling upgrade an older node would apply
	// what a newer one refuses, and the replicas would diverge.
	if s.raft.CurrentTerm() != term {
		return 0, false, fmt.Errorf("raft: WithTx: the leadership term changed before the write was proposed: %w", ErrNotLeader)
	}
	return 0, false, s.apply(tx.Ops())
}

// answer returns verdict, what fn made of a transaction that skipped the
// barrier of its term and proposes nothing: fn's error, or nil for a
// transaction that wrote nothing, once a quorum confirmed that this node still
// leads term, the term fn read in (confirmLeadership). A transaction that took
// the barrier needs no check: the barrier committed through a quorum in its
// term after the transaction began, which shows that no later term had
// committed anything before fn read. One that skipped it may run on a deposed
// leader, which still leads in its own view, and fn read a state that may lack
// what a later term committed, so verdict would be that of a stale state, such
// as the "not found" that a session revoke or a user delete takes for done.
//
// answer runs after the transaction released txMu, so the check's round trip
// holds up no other transaction. That leaves verdict as it was: fn read under
// txMu, so a write of this node that runs between the release and the answer is
// concurrent with the transaction, and comes after its read.
func (s *Store) answer(ctx context.Context, term uint64, verdict error) error {
	if err := confirmLeadership(ctx, s.raft, s.verifyLeader, term); err != nil {
		return err
	}
	return verdict
}

// confirmLeadership returns nil once verify, hashicorp/raft's VerifyLeader on
// r, confirmed that this node still leads, and the node leads term still: the
// term in which it read the state that the check stands for. VerifyLeader
// heartbeats the followers and writes nothing to the log; a follower in a later
// term answers that this node no longer leads, which steps it down, and a
// leader cut off from its quorum waits until its lease steps it down, within
// the bound of awaitLeadership. A check that fails is ErrNotLeader for a
// leadership lost, storage.ErrUnavailable for a node that shut down
// (barrierError). hashicorp/raft v1.8.0 counts for a check the answer to a
// request that a follower had in flight when the check began (notifyAll in its
// replication.go), so a deposed leader can still pass one within that round
// trip.
//
// A check that passes confirms the term the node was in when raft took it,
// which can be later than term: a leadership lost and regained since the read
// is in a later term, whose leaders may have committed what the read lacks. So
// the state, then the term, are read again, as awaitReady reads them. Terms
// only grow, so a node in term before the check and after it was in term when
// raft took the check, which confirmed it, and a node seen leading, then in
// term, still leads term. Any other answer is ErrNotLeader.
func confirmLeadership(ctx context.Context, r *hraft.Raft, verify func() error, term uint64) error {
	if err := awaitLeadership(ctx, verify); err != nil {
		return err
	}
	leading := r.State() == hraft.Leader
	if now := r.CurrentTerm(); !leading || now != term {
		return fmt.Errorf("raft: confirm the leadership: the term moved from %d to %d, leading: %v, since the state was read: %w", term, now, leading, ErrNotLeader)
	}
	return nil
}
