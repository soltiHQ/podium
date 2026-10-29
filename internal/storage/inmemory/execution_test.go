package inmemory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func archiveRow(t *testing.T, i int) *model.ExecutionRecord {
	t.Helper()
	e, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a", TaskName: fmt.Sprintf("task-%04d", i), TaskUID: "uid", Generation: 1, Attempt: 1, Status: "succeeded", SpecID: "spec", CapturedAt: time.Unix(int64(1000+i), 0)})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestExecutionTransactionPaginationAndFirstCapture(t *testing.T) {
	ctx := context.Background()
	s := New()
	first := archiveRow(t, 1)
	if err := s.UpsertExecution(ctx, first); err != nil {
		t.Fatal(err)
	}
	p := first.Params()
	p.CapturedAt = p.CapturedAt.Add(time.Hour)
	p.Error = "additional observation"
	p.Status = "failed"
	code := int32(88)
	p.ExitCode = &code
	replay, _ := model.NewExecutionRecord(p)
	if err := s.UpsertExecution(ctx, replay); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetExecution(ctx, first.ID())
	if !got.CapturedAt().Equal(first.CapturedAt()) || got.Error() != first.Error() || got.Status() != first.Status() || got.ExitCode() != nil {
		t.Fatal("first capture not immutable")
	}
	fail := errors.New("rollback")
	if err := s.WithTx(ctx, func(tx storage.Storage) error {
		if err := tx.DeleteExecution(ctx, first.ID()); err != nil {
			return err
		}
		if err := tx.UpsertExecution(ctx, archiveRow(t, 2)); err != nil {
			return err
		}
		return fail
	}); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if _, err := s.GetExecution(ctx, first.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExecution(ctx, archiveRow(t, 2).ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("failed transaction leaked", err)
	}
	if err := s.WithTx(ctx, func(tx storage.Storage) error {
		for i := 2; i <= 620; i++ {
			if err := tx.UpsertExecution(ctx, archiveRow(t, i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	statuses := []string{"succeeded"}
	f := s.BuildExecutionFilter(storage.ExecutionQueryCriteria{AgentID: "a", SpecID: "spec", TaskUID: "uid", Statuses: statuses})
	statuses[0] = "failed"
	rows, err := storage.AllExecutions(ctx, s, f)
	if err != nil || len(rows) != 620 {
		t.Fatal("pagination/filter loss", len(rows), err)
	}
	for i := 1; i < len(rows); i++ {
		if !rows[i-1].CapturedAt().After(rows[i].CapturedAt()) {
			t.Fatal("ordering mismatch")
		}
	}
	if _, err := s.ListExecutions(ctx, struct{}{}, storage.ListOptions{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatal("foreign filter accepted", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ListExecutions(ctx, nil, storage.ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
	snap := s.SnapshotForRaft()
	restored := New()
	restored.RestoreFromSnapshot(snap)
	rows, err = storage.AllExecutions(context.Background(), restored, nil)
	if err != nil || len(rows) != 620 {
		t.Fatal("snapshot omitted archive")
	}
}
