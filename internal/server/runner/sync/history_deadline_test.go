package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	historysvc "github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/storage"
)

type deadlineArchiveProxy struct {
	archiveProxy
	captureErr       error
	deleteContextErr error
}

func (f *deadlineArchiveProxy) GetTask(ctx context.Context, _ string) (*proxyv1.GetTaskResponse, error) {
	<-ctx.Done()
	f.captureErr = ctx.Err()
	return nil, ctx.Err()
}

func (f *deadlineArchiveProxy) DeleteTask(ctx context.Context, id string) error {
	f.deleteContextErr = ctx.Err()
	return f.archiveProxy.DeleteTask(ctx, id)
}

func TestArchiveDeadlinePreservesDeleteBudget(t *testing.T) {
	f := &deadlineArchiveProxy{}
	r, s, _ := newFakeRunner(t, &f.archiveProxy.fakeProxy)
	r.pool = archivePool{f}
	svc, err := historysvc.New(s, historysvc.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r.history = svc
	seedSpecAndAgent(t, s, "spec", "agent")
	rollout, err := model.NewRollout("spec", "agent", 1)
	if err != nil {
		t.Fatal(err)
	}
	rollout.SetActualTaskID("task")
	rollout.SetIntent(enum.RolloutIntentUninstall)
	if err := s.UpsertRollout(context.Background(), rollout); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	r.reconcileUninstall(ctx, rollout)
	if !errors.Is(f.captureErr, context.DeadlineExceeded) || f.deleteContextErr != nil || len(f.deletes) != 1 {
		t.Fatalf("capture=%v deleteCtx=%v deletes=%v", f.captureErr, f.deleteContextErr, f.deletes)
	}
	if _, err := s.GetRollout(context.Background(), rollout.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("uninstall incomplete: %v", err)
	}
	rows, err := storage.AllExecutions(context.Background(), s, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("timeout produced archive rows=%d err=%v", len(rows), err)
	}
}
