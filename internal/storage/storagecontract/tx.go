package storagecontract

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// transactions checks WithTx: the transaction reads its own writes and commits
// them, a callback error or a context done before the commit rolls every write
// back, a nested WithTx reuses the transaction, and the handle rotates sessions
// at the caller's time.
func transactions(t *testing.T, newStore storeFunc) {
	t.Run("reads its own writes and commits them", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		a, b, c := newWorld(t, "a", 0), newWorld(t, "b", 0), newWorld(t, "c", 0)
		must(t, errors.Join(a.write(ctx, s), b.write(ctx, s)))
		replaced := a.replacement(t, 1)

		calls := 0
		must(t, s.WithTx(ctx, func(tx storage.Storage) error {
			calls++
			if err := errors.Join(replaced.upserts(ctx, tx), c.write(ctx, tx), b.remove(ctx, tx)); err != nil {
				return err
			}
			replaced.in(t, "inside the transaction", tx)
			c.in(t, "inside the transaction", tx)
			b.out(t, "inside the transaction", tx)
			return nil
		}))
		if calls != 1 {
			t.Errorf("the callback ran %d times, want once", calls)
		}
		replaced.in(t, "after the commit", s)
		c.in(t, "after the commit", s)
		b.out(t, "after the commit", s)
	})

	t.Run("a callback error rolls every write back", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		a, b, c := newWorld(t, "a", 0), newWorld(t, "b", 0), newWorld(t, "c", 0)
		must(t, errors.Join(a.write(ctx, s), b.write(ctx, s)))
		replaced := a.replacement(t, 1)
		errCallback := errors.New("storagecontract: the callback fails")

		calls := 0
		err := s.WithTx(ctx, func(tx storage.Storage) error {
			calls++
			clocked, err := clockedHandle(tx)
			if err != nil {
				return err
			}
			// Every write method of the handle, once at least.
			if err := errors.Join(
				replaced.upserts(ctx, tx),
				c.write(ctx, tx),
				tx.RotateRefresh(ctx, a.session.ID(), []byte("rotated"), minute(60)),
				tx.RevokeSession(ctx, a.session.ID(), minute(30)),
				clocked.RotateRefreshAt(ctx, b.session.ID(), []byte("rotated at"), minute(90), minute(45)),
				tx.DeleteVerifierByCredential(ctx, a.credential.ID()),
				tx.DeleteSessionsByUser(ctx, a.user.ID()),
				tx.DeleteRolloutsBySpec(ctx, a.spec.ID()),
				b.remove(ctx, tx),
			); err != nil {
				return err
			}
			return errCallback
		})
		expect(t, outcome{"WithTx", err, errCallback})
		if calls != 1 {
			t.Errorf("the callback ran %d times, want once", calls)
		}
		a.in(t, "after the rollback", s)
		b.in(t, "after the rollback", s)
		c.out(t, "after the rollback", s)
	})

	t.Run("a context done before the commit rolls back", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		a, c := newWorld(t, "a", 0), newWorld(t, "c", 0)
		must(t, a.write(ctx, s))
		replaced := a.replacement(t, 1)

		// The callback returns nil once its context is done.
		txCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		err := s.WithTx(txCtx, func(tx storage.Storage) error {
			if err := errors.Join(replaced.upserts(txCtx, tx), c.write(txCtx, tx)); err != nil {
				return err
			}
			cancel()
			return nil
		})
		expect(t, outcome{"WithTx", err, context.Canceled})
		a.in(t, "after the cancelled commit", s)
		c.out(t, "after the cancelled commit", s)
	})

	t.Run("a context done before the transaction", func(t *testing.T) {
		// The spec says only that a context done before the commit rolls back. With
		// one done before WithTx, the in-memory store does not run the callback at
		// all, and the suite pins that.
		s := newStore(t)
		calls := 0
		err := s.WithTx(doneContext(t), func(storage.Storage) error {
			calls++
			return nil
		})
		expect(t, outcome{"WithTx", err, context.Canceled})
		if calls != 0 {
			t.Errorf("the callback ran %d times, want never", calls)
		}
	})

	t.Run("nested transaction", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		outer, inner, failed := fullAgent(t, "agent-a", 0, minute(0)), fullAgent(t, "agent-b", 0, minute(1)), fullAgent(t, "agent-c", 0, minute(2))
		errInner := errors.New("storagecontract: the nested callback fails")
		must(t, s.WithTx(ctx, func(tx storage.Storage) error {
			if err := tx.UpsertAgent(ctx, outer); err != nil {
				return err
			}
			// The nested transaction is the outer one: each sees the other's writes.
			if err := tx.WithTx(ctx, func(nested storage.Storage) error {
				if _, err := nested.GetAgent(ctx, outer.ID()); err != nil {
					return fmt.Errorf("the nested transaction misses the outer write: %w", err)
				}
				return nested.UpsertAgent(ctx, inner)
			}); err != nil {
				return err
			}
			if _, err := tx.GetAgent(ctx, inner.ID()); err != nil {
				return fmt.Errorf("the outer transaction misses the nested write: %w", err)
			}
			// There is no savepoint: the error of a nested callback rolls nothing back.
			err := tx.WithTx(ctx, func(nested storage.Storage) error {
				if err := nested.UpsertAgent(ctx, failed); err != nil {
					return err
				}
				return errInner
			})
			if !errors.Is(err, errInner) {
				return fmt.Errorf("the nested WithTx returned %v, want the error of its callback", err)
			}
			return nil
		}))
		for _, want := range []*model.Agent{outer, inner, failed} {
			got, err := s.GetAgent(ctx, want.ID())
			gotRow(t, "GetAgent after the commit", got, err, want, equalAgent)
		}

		// The outer transaction's error rolls the nested writes back.
		late := fullAgent(t, "agent-d", 0, minute(3))
		err := s.WithTx(ctx, func(tx storage.Storage) error {
			if err := tx.WithTx(ctx, func(nested storage.Storage) error { return nested.UpsertAgent(ctx, late) }); err != nil {
				return err
			}
			return errRollback
		})
		expect(t,
			outcome{"WithTx", err, errRollback},
			outcome{"GetAgent of the nested write rolled back", errOf(s.GetAgent(ctx, late.ID())), storage.ErrNotFound},
		)
	})

	t.Run("the handle rotates sessions at the caller's time", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		session := liveSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))
		must(t, s.CreateSession(ctx, session))
		want := session.Clone()
		must(t, want.RotateRefreshAt([]byte("rotated"), minute(60), minute(5)))

		must(t, s.WithTx(ctx, func(tx storage.Storage) error {
			clocked, err := clockedHandle(tx)
			if err != nil {
				return err
			}
			return clocked.RotateRefreshAt(ctx, session.ID(), []byte("rotated"), minute(60), minute(5))
		}))
		got, err := s.GetSession(ctx, session.ID())
		gotRow(t, "GetSession after the commit", got, err, want, equalSession)
	})

	t.Run("nil callback", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// The spec is silent on a nil callback: the in-memory store refuses it, in
		// a transaction too.
		expect(t, outcome{"WithTx of a nil callback", s.WithTx(ctx, nil), storage.ErrInvalidArgument})
		must(t, s.WithTx(ctx, func(tx storage.Storage) error {
			expect(t, outcome{"a nested WithTx of a nil callback", tx.WithTx(ctx, nil), storage.ErrInvalidArgument})
			return nil
		}))
	})
}

// clockedHandle returns the transaction handle as the ClockedSessionRotation the
// session service needs of it.
func clockedHandle(tx storage.Storage) (storage.ClockedSessionRotation, error) {
	clocked, ok := tx.(storage.ClockedSessionRotation)
	if !ok {
		return nil, errors.New("the transaction handle does not implement storage.ClockedSessionRotation")
	}
	return clocked, nil
}

// world is one row of every kind, linked as the services link them: the user's
// credential, its verifier and its session, the agent's credential and its
// execution record, and the spec's rollout on the agent.
type world struct {
	prefix     string
	agent      *model.Agent
	agentCred  *model.AgentCredential
	user       *model.User
	credential *model.Credential
	verifier   *model.Verifier
	session    *model.Session
	role       *model.Role
	spec       *model.Spec
	rollout    *model.Rollout
	execution  *model.ExecutionRecord
}

// newWorld returns the world of prefix with every field of every row set from
// v. Worlds of two prefixes share no key: ID, subject, name or link.
func newWorld(t *testing.T, prefix string, v int) world {
	t.Helper()
	var (
		created = minute(v)
		agent   = "agent-" + prefix
		user    = "user-" + prefix
		cred    = "cred-" + prefix
		spec    = "spec-" + prefix
	)
	return world{
		prefix:     prefix,
		agent:      fullAgent(t, agent, v, created),
		agentCred:  storedAgentCredential(t, agent, v, created),
		user:       fullUser(t, user, "sub-"+prefix, v, created),
		credential: fullCredential(t, cred, user, enum.Password, v, created),
		verifier:   fullVerifier(t, "ver-"+prefix, cred, v, created),
		session:    liveSession(t, "sess-"+prefix, user, cred, v, created),
		role:       fullRole(t, "role-"+prefix, "role "+prefix, v, created),
		spec:       fullSpec(t, spec, v, created),
		rollout:    fullRollout(t, spec, agent, v, created),
		execution:  fullExecution(t, agent, "uid-"+prefix, v, created),
	}
}

// replacement is w with every row a store replaces rebuilt from v. The session,
// which a store creates once, and the execution record, which it keeps as first
// written, stay those of w.
func (w world) replacement(t *testing.T, v int) world {
	t.Helper()
	r := newWorld(t, w.prefix, v)
	r.session, r.execution = w.session, w.execution
	return r
}

// upserts writes every row of w but the session, which a store does not upsert.
func (w world) upserts(ctx context.Context, s storage.Storage) error {
	return errors.Join(
		s.UpsertAgent(ctx, w.agent),
		s.UpsertAgentCredential(ctx, w.agentCred),
		s.UpsertUser(ctx, w.user),
		s.UpsertCredential(ctx, w.credential),
		s.UpsertVerifier(ctx, w.verifier),
		s.UpsertRole(ctx, w.role),
		s.UpsertSpec(ctx, w.spec),
		s.UpsertRollout(ctx, w.rollout),
		s.UpsertExecution(ctx, w.execution),
	)
}

// write writes every row of w.
func (w world) write(ctx context.Context, s storage.Storage) error {
	return errors.Join(w.upserts(ctx, s), s.CreateSession(ctx, w.session))
}

// remove deletes every row of w by its ID.
func (w world) remove(ctx context.Context, s storage.Storage) error {
	return errors.Join(
		s.DeleteAgent(ctx, w.agent.ID()),
		s.DeleteAgentCredential(ctx, w.agentCred.ID()),
		s.DeleteUser(ctx, w.user.ID()),
		s.DeleteCredential(ctx, w.credential.ID()),
		s.DeleteVerifier(ctx, w.verifier.ID()),
		s.DeleteSession(ctx, w.session.ID()),
		s.DeleteRole(ctx, w.role.ID()),
		s.DeleteSpec(ctx, w.spec.ID()),
		s.DeleteRollout(ctx, w.rollout.ID()),
		s.DeleteExecution(ctx, w.execution.ID()),
	)
}

// in checks that every read of s finds every row of w, equal to it.
func (w world) in(t *testing.T, what string, s storage.Storage) {
	t.Helper()
	ctx := t.Context()
	read := func(method string) string { return what + ": " + method }

	agent, err := s.GetAgent(ctx, w.agent.ID())
	gotRow(t, read("GetAgent"), agent, err, w.agent, equalAgent)
	agents, err := listedAgents(ctx, s, nil)
	inRows(t, read("ListAgents"), agents, err, w.agent, equalAgent)

	agentCred, err := s.GetAgentCredential(ctx, w.agentCred.ID())
	gotRow(t, read("GetAgentCredential"), agentCred, err, w.agentCred, equalAgentCredential)
	agentCreds, err := listedAgentCredentials(ctx, s)
	inRows(t, read("ListAgentCredentials"), agentCreds, err, w.agentCred, equalAgentCredential)

	user, err := s.GetUser(ctx, w.user.ID())
	gotRow(t, read("GetUser"), user, err, w.user, equalUser)
	user, err = s.GetUserBySubject(ctx, w.user.Subject())
	gotRow(t, read("GetUserBySubject"), user, err, w.user, equalUser)
	users, err := listedUsers(ctx, s, nil)
	inRows(t, read("ListUsers"), users, err, w.user, equalUser)

	credential, err := s.GetCredential(ctx, w.credential.ID())
	gotRow(t, read("GetCredential"), credential, err, w.credential, equalCredential)
	credential, err = s.GetCredentialByUserAndAuth(ctx, w.credential.UserID(), w.credential.AuthKind())
	gotRow(t, read("GetCredentialByUserAndAuth"), credential, err, w.credential, equalCredential)
	credentials, err := s.ListCredentialsByUser(ctx, w.credential.UserID())
	inRows(t, read("ListCredentialsByUser"), credentials, err, w.credential, equalCredential)

	verifier, err := s.GetVerifier(ctx, w.verifier.ID())
	gotRow(t, read("GetVerifier"), verifier, err, w.verifier, equalVerifier)
	verifier, err = s.GetVerifierByCredential(ctx, w.verifier.CredentialID())
	gotRow(t, read("GetVerifierByCredential"), verifier, err, w.verifier, equalVerifier)

	session, err := s.GetSession(ctx, w.session.ID())
	gotRow(t, read("GetSession"), session, err, w.session, equalSession)
	sessions, err := s.ListSessionsByUser(ctx, w.session.UserID())
	inRows(t, read("ListSessionsByUser"), sessions, err, w.session, equalSession)

	role, err := s.GetRole(ctx, w.role.ID())
	gotRow(t, read("GetRole"), role, err, w.role, equalRole)
	role, err = s.GetRoleByName(ctx, w.role.Name())
	gotRow(t, read("GetRoleByName"), role, err, w.role, equalRole)
	roles, err := s.GetRoles(ctx, []string{w.role.ID()})
	inRows(t, read("GetRoles"), roles, err, w.role, equalRole)
	roles, err = listedRoles(ctx, s, nil)
	inRows(t, read("ListRoles"), roles, err, w.role, equalRole)

	spec, err := s.GetSpec(ctx, w.spec.ID())
	gotRow(t, read("GetSpec"), spec, err, w.spec, equalSpec)
	specs, err := listedSpecs(ctx, s, nil)
	inRows(t, read("ListSpecs"), specs, err, w.spec, equalSpec)

	rollout, err := s.GetRollout(ctx, w.rollout.ID())
	gotRow(t, read("GetRollout"), rollout, err, w.rollout, equalRollout)
	rollouts, err := listedRollouts(ctx, s, nil)
	inRows(t, read("ListRollouts"), rollouts, err, w.rollout, equalRollout)

	execution, err := s.GetExecution(ctx, w.execution.ID())
	gotRow(t, read("GetExecution"), execution, err, w.execution, equalExecution)
	executions, err := listedExecutions(ctx, s, nil)
	inRows(t, read("ListExecutions"), executions, err, w.execution, equalExecution)
}

// out checks that no read of s finds a row of w.
func (w world) out(t *testing.T, what string, s storage.Storage) {
	t.Helper()
	ctx := t.Context()
	read := func(method string) string { return what + ": " + method }
	expect(t,
		outcome{read("GetAgent"), errOf(s.GetAgent(ctx, w.agent.ID())), storage.ErrNotFound},
		outcome{read("GetAgentCredential"), errOf(s.GetAgentCredential(ctx, w.agentCred.ID())), storage.ErrNotFound},
		outcome{read("GetUser"), errOf(s.GetUser(ctx, w.user.ID())), storage.ErrNotFound},
		outcome{read("GetUserBySubject"), errOf(s.GetUserBySubject(ctx, w.user.Subject())), storage.ErrNotFound},
		outcome{read("GetCredential"), errOf(s.GetCredential(ctx, w.credential.ID())), storage.ErrNotFound},
		outcome{read("GetCredentialByUserAndAuth"), errOf(s.GetCredentialByUserAndAuth(ctx, w.credential.UserID(), w.credential.AuthKind())), storage.ErrNotFound},
		outcome{read("GetVerifier"), errOf(s.GetVerifier(ctx, w.verifier.ID())), storage.ErrNotFound},
		outcome{read("GetVerifierByCredential"), errOf(s.GetVerifierByCredential(ctx, w.verifier.CredentialID())), storage.ErrNotFound},
		outcome{read("GetSession"), errOf(s.GetSession(ctx, w.session.ID())), storage.ErrNotFound},
		outcome{read("GetRole"), errOf(s.GetRole(ctx, w.role.ID())), storage.ErrNotFound},
		outcome{read("GetRoleByName"), errOf(s.GetRoleByName(ctx, w.role.Name())), storage.ErrNotFound},
		outcome{read("GetRoles"), errOf(s.GetRoles(ctx, []string{w.role.ID()})), storage.ErrNotFound},
		outcome{read("GetSpec"), errOf(s.GetSpec(ctx, w.spec.ID())), storage.ErrNotFound},
		outcome{read("GetRollout"), errOf(s.GetRollout(ctx, w.rollout.ID())), storage.ErrNotFound},
		outcome{read("GetExecution"), errOf(s.GetExecution(ctx, w.execution.ID())), storage.ErrNotFound},
	)
	agents, err := listedAgents(ctx, s, nil)
	notInRows(t, read("ListAgents"), agents, err, w.agent.ID())
	agentCreds, err := listedAgentCredentials(ctx, s)
	notInRows(t, read("ListAgentCredentials"), agentCreds, err, w.agentCred.ID())
	users, err := listedUsers(ctx, s, nil)
	notInRows(t, read("ListUsers"), users, err, w.user.ID())
	credentials, err := s.ListCredentialsByUser(ctx, w.credential.UserID())
	notInRows(t, read("ListCredentialsByUser"), credentials, err, w.credential.ID())
	sessions, err := s.ListSessionsByUser(ctx, w.session.UserID())
	notInRows(t, read("ListSessionsByUser"), sessions, err, w.session.ID())
	roles, err := listedRoles(ctx, s, nil)
	notInRows(t, read("ListRoles"), roles, err, w.role.ID())
	specs, err := listedSpecs(ctx, s, nil)
	notInRows(t, read("ListSpecs"), specs, err, w.spec.ID())
	rollouts, err := listedRollouts(ctx, s, nil)
	notInRows(t, read("ListRollouts"), rollouts, err, w.rollout.ID())
	executions, err := listedExecutions(ctx, s, nil)
	notInRows(t, read("ListExecutions"), executions, err, w.execution.ID())
}
