package model

import (
	"testing"

	"github.com/soltiHQ/control-plane/domain/kind"
)

// NewRollout starts as Pending with Intent=Install. This matches the
// first-deploy path: sync runner sees intent=Install and calls SubmitTask.
func TestNewRolloutDefaultsToInstallIntent(t *testing.T) {
	r, err := NewRollout("spec-1", "agent-1", 3)
	if err != nil {
		t.Fatalf("NewRollout: %v", err)
	}
	if got, want := r.Status(), kind.SyncStatusPending; got != want {
		t.Errorf("status: got %q, want %q", got, want)
	}
	if got, want := r.Intent(), kind.RolloutIntentInstall; got != want {
		t.Errorf("intent: got %q, want %q", got, want)
	}
	if got, want := r.DesiredGeneration(), 3; got != want {
		t.Errorf("desiredGeneration: got %d, want %d", got, want)
	}
	if r.ActualTaskID() != "" {
		t.Errorf("actualTaskID should be empty on fresh rollout, got %q", r.ActualTaskID())
	}
}

// MarkSynced clears the intent to Noop and records observedGeneration.
// This is the only path that transitions out of "actionable" — after
// MarkSynced the sync runner skips the rollout.
func TestMarkSyncedClearsIntent(t *testing.T) {
	r, _ := NewRollout("spec-1", "agent-1", 5)
	r.MarkSynced(2)

	if got, want := r.Status(), kind.SyncStatusSynced; got != want {
		t.Errorf("status: got %q, want %q", got, want)
	}
	if got, want := r.Intent(), kind.RolloutIntentNoop; got != want {
		t.Errorf("intent should be cleared to Noop after sync, got %q", got)
	}
	if got, want := r.ObservedGeneration(), 2; got != want {
		t.Errorf("observedGeneration: got %d, want %d", got, want)
	}
}

// MarkFailed leaves the intent intact — the sync runner must retry
// the same action on the next tick (bounded by MaxRetries).
func TestMarkFailedPreservesIntent(t *testing.T) {
	r, _ := NewRollout("spec-1", "agent-1", 1)
	r.SetIntent(kind.RolloutIntentUpdate)
	r.MarkFailed("boom")

	if got, want := r.Status(), kind.SyncStatusFailed; got != want {
		t.Errorf("status: got %q, want %q", got, want)
	}
	if got, want := r.Intent(), kind.RolloutIntentUpdate; got != want {
		t.Errorf("intent should stay Update after Failed, got %q", got)
	}
	if got, want := r.Attempts(), 1; got != want {
		t.Errorf("attempts: got %d, want %d", got, want)
	}
	if r.Error() != "boom" {
		t.Errorf("error: got %q, want %q", r.Error(), "boom")
	}
}

// SetActualTaskID is used during update both to record a fresh TaskId
// (after SubmitTask succeeds) and to clear it (between DeleteTask and
// SubmitTask for crash-safety).
func TestSetActualTaskIDSetAndClear(t *testing.T) {
	r, _ := NewRollout("spec-1", "agent-1", 1)
	r.SetActualTaskID("sub-slot-42")
	if got, want := r.ActualTaskID(), "sub-slot-42"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	r.SetActualTaskID("")
	if r.ActualTaskID() != "" {
		t.Errorf("empty string should clear, got %q", r.ActualTaskID())
	}
}

// Clone must be a full deep copy — mutations to the original after
// Clone must not leak into the copy. This test specifically covers the
// fields added in this change (intent, actualTaskID, observedGeneration).
func TestCloneIsIndependent(t *testing.T) {
	r, _ := NewRollout("spec-1", "agent-1", 1)
	r.SetActualTaskID("id-v1")
	r.MarkSynced(1)
	r.SetIntent(kind.RolloutIntentUpdate)

	cp := r.Clone()

	// Mutate the original; the clone must stay put.
	r.SetActualTaskID("id-v2")
	r.MarkFailed("downstream broke")
	r.SetIntent(kind.RolloutIntentUninstall)

	if cp.ActualTaskID() != "id-v1" {
		t.Errorf("clone.ActualTaskID leaked: got %q, want id-v1", cp.ActualTaskID())
	}
	if cp.Status() != kind.SyncStatusSynced {
		t.Errorf("clone.Status leaked: got %q, want Synced", cp.Status())
	}
	if cp.Intent() != kind.RolloutIntentUpdate {
		t.Errorf("clone.Intent leaked: got %q, want Update", cp.Intent())
	}
	if cp.Error() != "" {
		t.Errorf("clone.Error leaked: got %q", cp.Error())
	}
	if cp.ObservedGeneration() != 1 {
		t.Errorf("clone.ObservedGeneration leaked: got %d, want 1", cp.ObservedGeneration())
	}
}

// RolloutIntent.String returns stable lower-case labels. The UI renders
// badges by exact match, so regressions here are wire-observable.
func TestRolloutIntentStringIsStable(t *testing.T) {
	cases := map[kind.RolloutIntent]string{
		kind.RolloutIntentNoop:      "noop",
		kind.RolloutIntentInstall:   "install",
		kind.RolloutIntentUpdate:    "update",
		kind.RolloutIntentUninstall: "uninstall",
	}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("%v.String(): got %q, want %q", in, got, want)
		}
	}
}
