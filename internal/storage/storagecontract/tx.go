package storagecontract

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// transactions checks WithTx: the transaction reads its own writes and commits
// them, a callback error or a context done before the commit rolls every write
// back, a nested WithTx reuses the transaction, a write the transaction refuses
// leaves it usable, and the handle rotates sessions at the caller's time.
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

	t.Run("a refused write leaves the transaction usable", func(t *testing.T) {
		// The spec is silent on a write refused inside a transaction: the
		// in-memory store answers it, changes nothing for it and goes on, and the
		// suite pins that. The user service relies on it: its delete of a user
		// goes on after ErrNotFound and commits. Each write below is refused, or
		// matches no row, in a transaction that then writes an agent and commits,
		// so a database that aborts a transaction on a failed statement fails.
		var (
			session      = liveSession(t, "sess-a", "user-a", "cred-a", 0, minute(0))
			sameID       = fullSession(t, session.ID(), "user-b", "cred-b", 1, minute(5))
			deletedTwice = fullAgent(t, "agent-deleted", 0, minute(0))
			invalid      = fullAgent(t, "agent-invalid", 0, minute(0))
			absentRun    = fullExecution(t, "agent-x", "uid-x", 0, minute(0)).ID()
		)
		invalid.SetCreatedAt(time.Time{})
		for _, tc := range []struct {
			name  string
			want  error // nil, or the sentinel errors.Is finds in what the write answers
			write func(ctx context.Context, tx storage.Storage) error
		}{
			{"DeleteAgent of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error { return tx.DeleteAgent(ctx, "agent-x") }},
			{"DeleteAgentCredential of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error {
				return tx.DeleteAgentCredential(ctx, "agent-x")
			}},
			{"DeleteUser of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error { return tx.DeleteUser(ctx, "user-x") }},
			{"DeleteRole of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error { return tx.DeleteRole(ctx, "role-x") }},
			{"DeleteCredential of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error {
				return tx.DeleteCredential(ctx, "cred-x")
			}},
			{"DeleteVerifier of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error { return tx.DeleteVerifier(ctx, "ver-x") }},
			{"DeleteSession of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error { return tx.DeleteSession(ctx, "sess-x") }},
			{"DeleteSpec of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error { return tx.DeleteSpec(ctx, "spec-x") }},
			{"DeleteRollout of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error {
				return tx.DeleteRollout(ctx, model.RolloutID("spec-x", "agent-x"))
			}},
			{"DeleteExecution of an absent row", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error {
				return tx.DeleteExecution(ctx, absentRun)
			}},
			{"DeleteAgent of a row the transaction deleted", storage.ErrNotFound, func(ctx context.Context, tx storage.Storage) error {
				if err := errors.Join(tx.UpsertAgent(ctx, deletedTwice), tx.DeleteAgent(ctx, deletedTwice.ID())); err != nil {
					return fmt.Errorf("the writes before the second delete: %w", err)
				}
				return tx.DeleteAgent(ctx, deletedTwice.ID())
			}},
			{"DeleteVerifierByCredential of a credential with no verifier", nil, func(ctx context.Context, tx storage.Storage) error {
				return tx.DeleteVerifierByCredential(ctx, "cred-x")
			}},
			{"DeleteSessionsByUser of a user with no session", nil, func(ctx context.Context, tx storage.Storage) error {
				return tx.DeleteSessionsByUser(ctx, "user-x")
			}},
			{"DeleteRolloutsBySpec of a spec with no rollout", nil, func(ctx context.Context, tx storage.Storage) error {
				return tx.DeleteRolloutsBySpec(ctx, "spec-x")
			}},
			{"CreateSession of an existing ID", storage.ErrAlreadyExists, func(ctx context.Context, tx storage.Storage) error { return tx.CreateSession(ctx, sameID) }},
			{"UpsertAgent of a row without a creation time", storage.ErrInvalidArgument, func(ctx context.Context, tx storage.Storage) error {
				return tx.UpsertAgent(ctx, invalid)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s, ctx := newStore(t), t.Context()
				must(t, s.CreateSession(ctx, session))
				after := fullAgent(t, "agent-after", 0, minute(1))
				must(t, s.WithTx(ctx, func(tx storage.Storage) error {
					expect(t, outcome{tc.name + " inside the transaction", tc.write(ctx, tx), tc.want})
					return tx.UpsertAgent(ctx, after)
				}))
				got, err := s.GetAgent(ctx, after.ID())
				gotRow(t, "GetAgent of the write after the refused one", got, err, after, equalAgent)
				// The refused write changed nothing.
				kept, err := s.GetSession(ctx, session.ID())
				gotRow(t, "GetSession of the session written before the transaction", kept, err, session, equalSession)
				expect(t,
					outcome{"GetAgent of the row the transaction deleted", errOf(s.GetAgent(ctx, deletedTwice.ID())), storage.ErrNotFound},
					outcome{"GetAgent of the row without a creation time", errOf(s.GetAgent(ctx, invalid.ID())), storage.ErrNotFound},
				)
			})
		}
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

// clockedHandle returns the transaction handle as the ClockedSessionRotation
// the session service needs of it.
func clockedHandle(tx storage.Storage) (storage.ClockedSessionRotation, error) {
	clocked, ok := tx.(storage.ClockedSessionRotation)
	if !ok {
		return nil, errors.New("the transaction handle does not implement storage.ClockedSessionRotation")
	}
	return clocked, nil
}
