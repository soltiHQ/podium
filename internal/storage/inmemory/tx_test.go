package inmemory

import (
	"context"
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestWithTx_CommitOnSuccess(t *testing.T) {
	s := New()
	ctx := context.Background()

	err := s.WithTx(ctx, func(tx storage.Storage) error {
		return tx.UpsertAgent(ctx, mkAgent(t, "a1"))
	})
	requireNoErr(t, err)

	got, err := s.GetAgent(ctx, "a1")
	requireNoErr(t, err)
	if got.ID() != "a1" {
		t.Fatalf("id: want a1, got %s", got.ID())
	}
}

func TestWithTx_RollbackOnError(t *testing.T) {
	s := New()
	ctx := context.Background()

	boom := errors.New("boom")
	err := s.WithTx(ctx, func(tx storage.Storage) error {
		_ = tx.UpsertAgent(ctx, mkAgent(t, "a1"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if _, err := s.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestWithTx_PreexistingDataSurvivesRollback(t *testing.T) {
	s := New()
	ctx := context.Background()
	requireNoErr(t, s.UpsertAgent(ctx, mkAgent(t, "existing")))

	_ = s.WithTx(ctx, func(tx storage.Storage) error {
		_ = tx.UpsertAgent(ctx, mkAgent(t, "new"))
		_ = tx.DeleteAgent(ctx, "existing")
		return errors.New("abort")
	})

	if _, err := s.GetAgent(ctx, "existing"); err != nil {
		t.Fatalf("existing lost: %v", err)
	}
	if _, err := s.GetAgent(ctx, "new"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("new leaked: %v", err)
	}
}

func TestWithTx_NestedCallsDoNotDeadlock(t *testing.T) {
	s := New()
	ctx := context.Background()

	err := s.WithTx(ctx, func(tx storage.Storage) error {
		return tx.WithTx(ctx, func(tx2 storage.Storage) error {
			return tx2.UpsertAgent(ctx, mkAgent(t, "nested"))
		})
	})
	requireNoErr(t, err)

	if _, err := s.GetAgent(ctx, "nested"); err != nil {
		t.Fatalf("nested agent missing: %v", err)
	}
}
