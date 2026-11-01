package raft_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/durable"
)

// deleteOutcome is what a store made of a transaction that deleted a row and then,
// unless the delete failed with anything but ErrNotFound, wrote an agent.
type deleteOutcome struct {
	deleted   string // "ok", "not found" or the error of the delete
	committed string // "ok" or the error of the transaction
	written   bool   // whether the agent is there after the transaction
}

func outcomeOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, storage.ErrNotFound):
		return "not found"
	default:
		return err.Error()
	}
}

// deleteThenWrite runs the transaction deleteOutcome describes on s, writing the
// agent marker.
func deleteThenWrite(t *testing.T, s storage.Storage, marker string, del func(context.Context, storage.Storage) error) deleteOutcome {
	t.Helper()
	ctx := t.Context()
	a, err := model.NewAgent(marker, marker, "http://"+marker)
	if err != nil {
		t.Fatal(err)
	}
	var got deleteOutcome
	err = s.WithTx(ctx, func(tx storage.Storage) error {
		err := del(ctx, tx)
		got.deleted = outcomeOf(err)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		return tx.UpsertAgent(ctx, a)
	})
	got.committed = outcomeOf(err)
	if _, err := s.GetAgent(ctx, marker); err == nil {
		got.written = true
	} else if !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	return got
}

// TestRaftDeleteOfAbsentRowMatchesStandalone deletes, in a transaction, a row the
// transaction does not see, and goes on to write another. Standalone answers the
// delete with ErrNotFound, changes nothing for it and commits the write; Raft must
// do the same, rather than record the delete and fail the whole command when the
// state machine applies it. A bulk delete that matches nothing succeeds on both.
func TestRaftDeleteOfAbsentRowMatchesStandalone(t *testing.T) {
	standalone, err := durable.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = standalone.Close() })
	replicated := newSingleNode(t).Store()

	for _, tc := range []struct {
		name    string
		deleted string // what both stores must answer the delete with
		del     func(context.Context, storage.Storage) error
	}{
		{"DeleteAgent", "not found", func(ctx context.Context, tx storage.Storage) error { return tx.DeleteAgent(ctx, "absent") }},
		{"DeleteAgentCredential", "not found", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteAgentCredential(ctx, "absent")
		}},
		{"DeleteUser", "not found", func(ctx context.Context, tx storage.Storage) error { return tx.DeleteUser(ctx, "absent") }},
		{"DeleteRole", "not found", func(ctx context.Context, tx storage.Storage) error { return tx.DeleteRole(ctx, "absent") }},
		{"DeleteCredential", "not found", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteCredential(ctx, "absent")
		}},
		{"DeleteVerifier", "not found", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteVerifier(ctx, "absent")
		}},
		{"DeleteSession", "not found", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteSession(ctx, "absent")
		}},
		{"DeleteSpec", "not found", func(ctx context.Context, tx storage.Storage) error { return tx.DeleteSpec(ctx, "absent") }},
		{"DeleteRollout", "not found", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteRollout(ctx, "absent")
		}},
		{"DeleteExecution", "not found", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteExecution(ctx, "absent")
		}},
		{"DeleteAgent of a row the transaction deleted", "not found", func(ctx context.Context, tx storage.Storage) error {
			a, err := model.NewAgent("deleted-twice", "deleted twice", "http://deleted-twice")
			if err != nil {
				return err
			}
			if err := tx.UpsertAgent(ctx, a); err != nil {
				return err
			}
			if err := tx.DeleteAgent(ctx, a.ID()); err != nil {
				return err
			}
			return tx.DeleteAgent(ctx, a.ID())
		}},
		{"DeleteVerifierByCredential", "ok", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteVerifierByCredential(ctx, "absent")
		}},
		{"DeleteSessionsByUser", "ok", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteSessionsByUser(ctx, "absent")
		}},
		{"DeleteRolloutsBySpec", "ok", func(ctx context.Context, tx storage.Storage) error {
			return tx.DeleteRolloutsBySpec(ctx, "absent")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := "marker-" + filepath.Base(t.Name())
			want := deleteOutcome{deleted: tc.deleted, committed: "ok", written: true}
			if got := deleteThenWrite(t, standalone, marker, tc.del); got != want {
				t.Fatalf("standalone: %+v, want %+v", got, want)
			}
			if got := deleteThenWrite(t, replicated, marker, tc.del); got != want {
				t.Fatalf("raft: %+v, want what standalone made of it, %+v", got, want)
			}
		})
	}
}
