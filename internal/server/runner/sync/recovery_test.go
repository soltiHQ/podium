package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
)

func TestApplyCompletionPreservesConcurrentDelete(t *testing.T) {
	fp := &fakeProxy{applyResp: []string{"new-task"}}
	r, s, _ := newFakeRunner(t, fp)
	ts := seedSpecAndAgent(t, s, "race", "agent")
	ro, _ := model.NewRollout(ts.ID(), "agent", 1)
	_ = s.UpsertRollout(context.Background(), ro)
	fp.onApply = func() {
		current, _ := s.GetRollout(context.Background(), ro.ID())
		current.SetIntent(enum.RolloutIntentUninstall)
		current.MarkPending(1)
		_ = s.UpsertRollout(context.Background(), current)
	}
	r.reconcile(context.Background(), ro.ID())
	after, _ := s.GetRollout(context.Background(), ro.ID())
	if after.Intent() != enum.RolloutIntentUninstall || after.ActualTaskID() != "new-task" || after.Status() != enum.SyncStatusPending {
		t.Fatal("apply resurrected deleted target")
	}
	r.reconcile(context.Background(), ro.ID())
	if _, err := s.GetRollout(context.Background(), ro.ID()); err == nil {
		t.Fatal("in-flight applied task not drained")
	}
}

func TestFailedApplyDoesNotOverwriteConcurrentDeploy(t *testing.T) {
	fp := &fakeProxy{applyErr: []error{errors.New("offline")}}
	r, s, _ := newFakeRunner(t, fp)
	ts := seedSpecAndAgent(t, s, "race", "agent")
	ro, _ := model.NewRollout(ts.ID(), "agent", 1)
	_ = s.UpsertRollout(context.Background(), ro)
	fp.onApply = func() {
		current, _ := s.GetRollout(context.Background(), ro.ID())
		current.SetIntent(enum.RolloutIntentUpdate)
		current.MarkPending(2)
		_ = s.UpsertRollout(context.Background(), current)
	}
	r.reconcile(context.Background(), ro.ID())
	after, _ := s.GetRollout(context.Background(), ro.ID())
	if after.Status() != enum.SyncStatusPending || after.DesiredGeneration() != 2 || after.Attempts() != 0 {
		t.Fatal("old failure overwrote newer deployment")
	}
}

func TestDeleteCompletionPreservesReaddedTarget(t *testing.T) {
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	ts := seedSpecAndAgent(t, s, "race", "agent")
	ro, _ := model.NewRollout(ts.ID(), "agent", 1)
	ro.SetActualTaskID("old")
	ro.SetIntent(enum.RolloutIntentUninstall)
	_ = s.UpsertRollout(context.Background(), ro)
	fp.onDelete = func() {
		current, _ := s.GetRollout(context.Background(), ro.ID())
		current.SetIntent(enum.RolloutIntentUpdate)
		current.MarkPending(2)
		_ = s.UpsertRollout(context.Background(), current)
	}
	r.reconcile(context.Background(), ro.ID())
	after, err := s.GetRollout(context.Background(), ro.ID())
	if err != nil || after.Intent() != enum.RolloutIntentInstall || after.ActualTaskID() != "" || after.DesiredGeneration() != 2 {
		t.Fatal("delete completion lost re-added target")
	}
}

func TestObservationRecoversAgentRestart(t *testing.T) {
	fp := &fakeProxy{getErr: agentNotFound{}}
	r, s, _ := newFakeRunner(t, fp)
	ts := seedSpecAndAgent(t, s, "restart", "agent")
	ro, _ := model.NewRollout(ts.ID(), "agent", 1)
	ro.SetActualTaskID("lost")
	ro.MarkSynced(1)
	_ = s.UpsertRollout(context.Background(), ro)
	r.reconcile(context.Background(), ro.ID())
	after, _ := s.GetRollout(context.Background(), ro.ID())
	if after.Intent() != enum.RolloutIntentInstall || after.Status() != enum.SyncStatusPending || after.ActualTaskID() != "" {
		t.Fatal("lost agent task not queued for recovery")
	}
}

func TestFinalizeScansPastFirst500Specs(t *testing.T) {
	r, s, _ := newFakeRunner(t, &fakeProxy{})
	ctx := context.Background()
	for i := 0; i < 503; i++ {
		ts, _ := model.NewSpec(fmt.Sprintf("sp-%04d", i), "n", "slot")
		if i == 0 {
			ts.MarkForDeletion()
		}
		_ = s.UpsertSpec(ctx, ts)
	}
	r.finalizeDeletedSpecs(ctx)
	if _, err := s.GetSpec(ctx, "sp-0000"); err == nil {
		t.Fatal("oldest tombstone beyond first page was ignored")
	}
}

func TestEditedSpecDoesNotApplyWithoutNewDeployment(t *testing.T) {
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	ts := seedSpecAndAgent(t, s, "edit", "agent")
	ro, _ := model.NewRollout(ts.ID(), "agent", 1)
	_ = s.UpsertRollout(context.Background(), ro)
	ts.BumpGeneration()
	_ = s.UpsertSpec(context.Background(), ts)
	r.reconcile(context.Background(), ro.ID())
	after, _ := s.GetRollout(context.Background(), ro.ID())
	if len(fp.applies) != 0 || after.Status() != enum.SyncStatusFailed {
		t.Fatal("un-deployed newer generation was applied")
	}
}

func TestSpecEditDuringApplyLeavesAppliedGenerationSynced(t *testing.T) {
	fp := &fakeProxy{applyResp: []string{"task"}}
	r, s, _ := newFakeRunner(t, fp)
	ts := seedSpecAndAgent(t, s, "edit", "agent")
	ro, _ := model.NewRollout(ts.ID(), "agent", 1)
	_ = s.UpsertRollout(context.Background(), ro)
	fp.onApply = func() { ts.BumpGeneration(); _ = s.UpsertSpec(context.Background(), ts) }
	r.reconcile(context.Background(), ro.ID())
	after, _ := s.GetRollout(context.Background(), ro.ID())
	if after.Status() != enum.SyncStatusSynced || after.ObservedGeneration() != 1 || after.DesiredGeneration() != 1 {
		t.Fatal("edit without deploy caused repeated apply")
	}
}
