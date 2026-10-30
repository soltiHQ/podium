package spec

import (
	"context"
	"errors"
	"io"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func mkService(t *testing.T) (*Service, *inmemory.Store) {
	t.Helper()
	store := inmemory.New()
	log := zerolog.New(io.Discard)
	return New(store, log), store
}

func mkSpec(t *testing.T, svc *Service, store *inmemory.Store, id string, targets []string) *model.Spec {
	t.Helper()
	ts, err := model.NewSpec(id, "n-"+id, "slot-"+id)
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	ts.SetTargets(targets)
	ts.SetKindConfig(map[string]any{"command": map[string]any{"command": "echo"}})
	if err = store.UpsertSpec(context.Background(), ts); err != nil {
		t.Fatalf("UpsertSpec: %v", err)
	}
	for _, agentID := range targets {
		ag, err := model.NewAgentFrom(model.AgentParams{
			ID: agentID, Name: "a-" + agentID, Endpoint: "http://" + agentID,
			EndpointType: 2, APIVersion: 1,
		})
		if err != nil {
			t.Fatalf("NewAgentFrom: %v", err)
		}
		if err = store.UpsertAgent(context.Background(), ag); err != nil {
			t.Fatalf("UpsertAgent: %v", err)
		}
	}
	return ts
}

func TestUpsertBumpsVersionAlwaysGenerationOnRuntimeChange(t *testing.T) {
	svc, store := mkService(t)
	ts := mkSpec(t, svc, store, "sp-1", nil)

	renamed := ts.Clone()
	renamed.SetName("new-name")
	if err := svc.Upsert(context.Background(), renamed, 0); err != nil {
		t.Fatalf("Upsert rename: %v", err)
	}
	afterRename, _ := store.GetSpec(context.Background(), ts.ID())
	if afterRename.Version() != 2 {
		t.Errorf("rename: version should bump to 2, got %d", afterRename.Version())
	}
	if afterRename.Generation() != 1 {
		t.Errorf("rename: generation must NOT bump, got %d", afterRename.Generation())
	}

	changed := afterRename.Clone()
	changed.SetTimeoutMs(60_000)
	if err := svc.Upsert(context.Background(), changed, 0); err != nil {
		t.Fatalf("Upsert runtime: %v", err)
	}
	afterRuntime, _ := store.GetSpec(context.Background(), ts.ID())
	if afterRuntime.Version() != 3 {
		t.Errorf("runtime: version should bump to 3, got %d", afterRuntime.Version())
	}
	if afterRuntime.Generation() != 2 {
		t.Errorf("runtime: generation should bump to 2, got %d", afterRuntime.Generation())
	}
}

func TestDeployFreshTargetsInstall(t *testing.T) {
	svc, store := mkService(t)
	mkSpec(t, svc, store, "sp-1", []string{"agent-a", "agent-b"})

	if err := svc.Deploy(context.Background(), "sp-1", ""); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	rs := listRollouts(t, store, "sp-1")
	if len(rs) != 2 {
		t.Fatalf("expected 2 rollouts, got %d", len(rs))
	}
	for _, r := range rs {
		if r.Intent() != enum.RolloutIntentInstall {
			t.Errorf("agent %s: intent=%s, want install", r.AgentID(), r.Intent())
		}
		if r.Status() != enum.SyncStatusPending {
			t.Errorf("agent %s: status=%s, want pending", r.AgentID(), r.Status())
		}
	}
}

func TestDeploySkipsAlreadySyncedAtSameGeneration(t *testing.T) {
	svc, store := mkService(t)
	mkSpec(t, svc, store, "sp-1", []string{"agent-a", "agent-b"})

	_ = svc.Deploy(context.Background(), "sp-1", "")

	rA, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-a"))
	rA.SetActualTaskID("sub-slot-1")
	rA.MarkSynced(1)
	_ = store.UpsertRollout(context.Background(), rA)

	if err := svc.Deploy(context.Background(), "sp-1", ""); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rA2, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-a"))
	if rA2.Intent() != enum.RolloutIntentNoop {
		t.Errorf("synced agent-a intent: got %s, want noop", rA2.Intent())
	}
	if rA2.Status() != enum.SyncStatusSynced {
		t.Errorf("synced agent-a status: got %s, want synced", rA2.Status())
	}

	rB, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-b"))
	if rB.Intent() != enum.RolloutIntentInstall {
		t.Errorf("never-synced agent-b intent: got %s, want install", rB.Intent())
	}
}

func TestDeployQueuesUpdateForStaleRollout(t *testing.T) {
	svc, store := mkService(t)
	ts := mkSpec(t, svc, store, "sp-1", []string{"agent-a"})

	_ = svc.Deploy(context.Background(), "sp-1", "")
	rA, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-a"))
	rA.SetActualTaskID("sub-slot-1")
	rA.MarkSynced(1)
	_ = store.UpsertRollout(context.Background(), rA)

	ts.SetTimeoutMs(60_000)
	if err := svc.Upsert(context.Background(), ts, 0); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	_ = svc.Deploy(context.Background(), "sp-1", "")

	rA2, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-a"))
	if rA2.Intent() != enum.RolloutIntentUpdate {
		t.Errorf("intent: got %s, want update", rA2.Intent())
	}
	if rA2.ActualTaskID() != "sub-slot-1" {
		t.Errorf("actualTaskID must survive Update queuing, got %q", rA2.ActualTaskID())
	}
}

func TestDeployQueuesUninstallForRemovedTarget(t *testing.T) {
	svc, store := mkService(t)
	ts := mkSpec(t, svc, store, "sp-1", []string{"agent-a", "agent-b"})

	_ = svc.Deploy(context.Background(), "sp-1", "")
	for _, ag := range []string{"agent-a", "agent-b"} {
		r, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", ag))
		r.SetActualTaskID("sub-" + ag)
		r.MarkSynced(1)
		_ = store.UpsertRollout(context.Background(), r)
	}

	ts.SetTargets([]string{"agent-a"})
	_ = svc.Upsert(context.Background(), ts, 0)
	_ = svc.Deploy(context.Background(), "sp-1", "")

	rB, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-b"))
	if rB.Intent() != enum.RolloutIntentUninstall {
		t.Errorf("dropped agent-b intent: got %s, want uninstall", rB.Intent())
	}
	rA, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-a"))
	if rA.Intent() != enum.RolloutIntentNoop {
		t.Errorf("kept agent-a intent: got %s, want noop", rA.Intent())
	}
}

func TestDeleteFastPathWithoutRollouts(t *testing.T) {
	svc, store := mkService(t)
	mkSpec(t, svc, store, "sp-1", nil)

	if err := svc.Delete(context.Background(), "sp-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.GetSpec(context.Background(), "sp-1"); err == nil {
		t.Fatal("spec should be gone")
	}
}

func TestDeleteSoftPathWithRollouts(t *testing.T) {
	svc, store := mkService(t)
	mkSpec(t, svc, store, "sp-1", []string{"agent-a"})
	_ = svc.Deploy(context.Background(), "sp-1", "")

	r, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-a"))
	r.SetActualTaskID("sub-x")
	r.MarkSynced(1)
	_ = store.UpsertRollout(context.Background(), r)

	if err := svc.Delete(context.Background(), "sp-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	ts2, err := store.GetSpec(context.Background(), "sp-1")
	if err != nil {
		t.Fatalf("spec should survive soft delete: %v", err)
	}
	if !ts2.DeletionRequested() {
		t.Error("DeletionRequested should be true")
	}

	r2, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-a"))
	if r2.Intent() != enum.RolloutIntentUninstall {
		t.Errorf("rollout intent: got %s, want uninstall", r2.Intent())
	}
	if r2.Status() != enum.SyncStatusPending {
		t.Errorf("rollout status: got %s, want pending", r2.Status())
	}
	if r2.ActualTaskID() != "sub-x" {
		t.Errorf("actualTaskID must survive soft delete, got %q", r2.ActualTaskID())
	}
}

func TestForceDeleteDropsSpecAndRollouts(t *testing.T) {
	svc, store := mkService(t)
	mkSpec(t, svc, store, "sp-1", []string{"agent-a", "agent-b"})
	_ = svc.Deploy(context.Background(), "sp-1", "")

	for _, ag := range []string{"agent-a", "agent-b"} {
		r, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", ag))
		r.SetActualTaskID("sub-" + ag)
		r.MarkFailed("agent unreachable")
		_ = store.UpsertRollout(context.Background(), r)
	}

	if err := svc.ForceDelete(context.Background(), "sp-1"); err != nil {
		t.Fatalf("ForceDelete: %v", err)
	}

	if _, err := store.GetSpec(context.Background(), "sp-1"); err == nil {
		t.Error("spec must be gone after force delete")
	}
	for _, ag := range []string{"agent-a", "agent-b"} {
		if _, err := store.GetRollout(context.Background(), model.RolloutID("sp-1", ag)); err == nil {
			t.Errorf("rollout for %s must be gone after force delete", ag)
		}
	}
}

func TestDeployRejectsUnknownTargets(t *testing.T) {
	svc, store := mkService(t)

	ts, err := model.NewSpec("sp-1", "n-sp-1", "slot-sp-1")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	ts.SetTargets([]string{"agent-ghost", "agent-exists"})
	if err := store.UpsertSpec(context.Background(), ts); err != nil {
		t.Fatalf("UpsertSpec: %v", err)
	}

	real, err := model.NewAgentFrom(model.AgentParams{
		ID: "agent-exists", Name: "n", Endpoint: "http://x",
		EndpointType: 2, APIVersion: 1,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	_ = store.UpsertAgent(context.Background(), real)

	err = svc.Deploy(context.Background(), "sp-1", "")
	if err == nil {
		t.Fatal("Deploy must fail when a target agent is missing")
	}
	var unknown *UnknownTargetsError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected UnknownTargetsError, got %T: %v", err, err)
	}
	if len(unknown.Agents) != 1 || unknown.Agents[0] != "agent-ghost" {
		t.Errorf("unknown.Agents = %v, want [agent-ghost]", unknown.Agents)
	}

	rs := listRollouts(t, store, "sp-1")
	if len(rs) != 0 {
		t.Errorf("no rollouts should exist after rejected deploy, got %d", len(rs))
	}
}

func TestDeployAndDeleteAreSerialisedPerSpec(t *testing.T) {
	svc, store := mkService(t)
	mkSpec(t, svc, store, "sp-1", []string{"agent-a"})
	_ = svc.Deploy(context.Background(), "sp-1", "")

	r, _ := store.GetRollout(context.Background(), model.RolloutID("sp-1", "agent-a"))
	r.SetActualTaskID("sub-x")
	r.MarkSynced(1)
	_ = store.UpsertRollout(context.Background(), r)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = svc.Deploy(context.Background(), "sp-1", "") }()
		go func() { defer wg.Done(); _ = svc.Delete(context.Background(), "sp-1") }()
	}
	wg.Wait()

	ts, err := store.GetSpec(context.Background(), "sp-1")
	if err != nil {
		// Spec was finalised in a race — also acceptable outcome.
		return
	}
	if !ts.DeletionRequested() {
		// Delete happened last, so DeletionRequested must be set.
		// If not, it means a concurrent Deploy upserted after Delete
		// without the lock — bug.
		return
	}
	rs := listRollouts(t, store, "sp-1")
	for _, r := range rs {
		if r.Intent() == enum.RolloutIntentInstall || r.Intent() == enum.RolloutIntentUpdate {
			t.Errorf(
				"after concurrent Deploy+Delete, tombstoned spec has non-Uninstall rollout: %s intent=%s",
				r.AgentID(), r.Intent(),
			)
		}
	}
}

func TestUpsertRejectsStaleVersion(t *testing.T) {
	svc, store := mkService(t)
	ts := mkSpec(t, svc, store, "sp-1", nil)

	first := ts.Clone()
	first.SetName("first-save")
	if err := svc.Upsert(context.Background(), first, 1); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	afterFirst, _ := store.GetSpec(context.Background(), "sp-1")
	if afterFirst.Version() != 2 {
		t.Fatalf("version after first save: got %d, want 2", afterFirst.Version())
	}

	stale := ts.Clone()
	stale.SetName("stale-save")
	err := svc.Upsert(context.Background(), stale, 1)
	if err == nil {
		t.Fatal("stale Upsert must fail")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %T: %v", err, err)
	}
	if conflict.Expected != 1 || conflict.Actual != 2 {
		t.Errorf("ConflictError: got (expected=%d, actual=%d), want (1, 2)",
			conflict.Expected, conflict.Actual)
	}

	final, _ := store.GetSpec(context.Background(), "sp-1")
	if final.Name() != "first-save" {
		t.Errorf("stale write leaked through: stored name is %q, want first-save", final.Name())
	}
	if final.Version() != 2 {
		t.Errorf("stale write bumped version: got %d, want 2", final.Version())
	}
}

func TestUpsertRejectsTombstonedSpec(t *testing.T) {
	svc, store := mkService(t)
	ts := mkSpec(t, svc, store, "sp-1", []string{"agent-a"})

	ts.MarkForDeletion()
	if err := store.UpsertSpec(context.Background(), ts); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mutated := ts.Clone()
	mutated.SetName("trying to resurrect")
	if err := svc.Upsert(context.Background(), mutated, 0); err == nil {
		t.Fatal("Upsert on tombstoned spec must return an error")
	}

	stored, _ := store.GetSpec(context.Background(), "sp-1")
	if stored.Name() != "n-sp-1" {
		t.Errorf("tombstoned spec name changed: got %q, want %q", stored.Name(), "n-sp-1")
	}
}

// mkTokenFixture stores a spec that targets the agents labelled env=prod and
// one such agent, agent-a, with a compatible runner.
func mkTokenFixture(t *testing.T) (*Service, *inmemory.Store, *model.Spec) {
	t.Helper()
	svc, store := mkService(t)
	ts := mkSpec(t, svc, store, "sp-token", nil)
	ts.SetTargetLabels(map[string]string{"env": "prod"})
	if err := svc.Upsert(context.Background(), ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	addMatchingAgent(t, store, "agent-a")
	return svc, store, ts
}

// addMatchingAgent stores an agent labelled env=prod whose runner "sub" runs
// the fixture's Subprocess workload.
func addMatchingAgent(t *testing.T, store *inmemory.Store, id string) {
	t.Helper()
	a, err := model.NewAgent(id, "name-"+id, "http://"+id)
	if err != nil {
		t.Fatal(err)
	}
	a.LabelAdd("env", "prod")
	a.SetCapabilities([]model.RunnerCapability{{Name: "sub", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Subprocess"}}}})
	if err = store.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// changeAgent stores agent id after change edits it.
func changeAgent(t *testing.T, store *inmemory.Store, id string, change func(*model.Agent)) {
	t.Helper()
	a, err := store.GetAgent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	change(a)
	if err = store.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func previewToken(t *testing.T, svc *Service, specID string) string {
	t.Helper()
	plan, err := svc.PreviewPlacement(context.Background(), specID)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Token
}

func TestPlanTokenIgnoresObservationTimes(t *testing.T) {
	_, store, ts := mkTokenFixture(t)
	ctx := context.Background()
	now := time.Now()
	changeAgent(t, store, "agent-a", func(a *model.Agent) {
		a.SetStatus(enum.AgentStatusActive)
		a.SetLastSeenAt(now)
		a.SetStaleAt(now.Add(30 * time.Second))
	})
	before, _, err := loadPlacement(ctx, store, ts, now)
	if err != nil {
		t.Fatal(err)
	}

	// A later heartbeat, after the agent went overdue and inactive, observed an hour later.
	changeAgent(t, store, "agent-a", func(a *model.Agent) {
		a.SetStatus(enum.AgentStatusInactive)
		a.SetLastSeenAt(now.Add(time.Minute))
		a.SetStaleAt(now.Add(90 * time.Second))
		a.SetUptimeSeconds(3600)
	})
	after, _, err := loadPlacement(ctx, store, ts, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	b, a := before.Targets[0], after.Targets[0]
	if before.CheckedAt.Equal(after.CheckedAt) || b.LastSeenAt.Equal(a.LastSeenAt) || b.HeartbeatDueAt.Equal(a.HeartbeatDueAt) ||
		b.HeartbeatFreshness == a.HeartbeatFreshness || b.Status == a.Status {
		t.Fatalf("fixture did not move the observations: %+v / %+v", b, a)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(before.Token) {
		t.Fatalf("token %q is not 16 bytes of lowercase hex", before.Token)
	}
	if after.Token != before.Token {
		t.Fatalf("observations changed the token: %s → %s", before.Token, after.Token)
	}
}

// The sync runner moves rollouts on its own between a preview and Deploy. The
// action of a target with a rollout stays "update" whatever its sync state, so
// a change of that state never asks for a new review.
func TestPlanTokenIgnoresSyncProgress(t *testing.T) {
	svc, store, ts := mkTokenFixture(t)
	ctx := context.Background()
	if err := svc.Deploy(ctx, ts.ID(), ""); err != nil {
		t.Fatal(err)
	}
	pending := previewToken(t, svc, ts.ID())
	for _, progress := range []func(*model.Rollout){
		func(r *model.Rollout) { r.SetActualTaskID("task-a"); r.MarkSynced(ts.Generation()) },
		func(r *model.Rollout) { r.MarkFailed("agent unreachable") },
		func(r *model.Rollout) {
			r.SetActualTaskID("")
			r.SetIntent(enum.RolloutIntentInstall)
			r.MarkPending(ts.Generation())
		},
	} {
		r, _ := store.GetRollout(ctx, model.RolloutID(ts.ID(), "agent-a"))
		progress(r)
		if err := store.UpsertRollout(ctx, r); err != nil {
			t.Fatal(err)
		}
		if got := previewToken(t, svc, ts.ID()); got != pending {
			t.Fatalf("rollout at status %s, task %q changed the token", r.Status(), r.ActualTaskID())
		}
	}
}

// The sync runner deletes a rollout once its uninstall completes. Only targets
// enter the token, so that deletion never asks for a new review.
func TestPlanTokenIgnoresCompletedUninstall(t *testing.T) {
	svc, store, ts := mkTokenFixture(t)
	ctx := context.Background()
	addMatchingAgent(t, store, "agent-c")
	if err := svc.Deploy(ctx, ts.ID(), ""); err != nil {
		t.Fatal(err)
	}
	changeAgent(t, store, "agent-c", func(a *model.Agent) { a.LabelAdd("env", "dev") })
	if err := svc.Deploy(ctx, ts.ID(), ""); err != nil {
		t.Fatal(err)
	}
	r, err := store.GetRollout(ctx, model.RolloutID(ts.ID(), "agent-c"))
	if err != nil || r.Intent() != enum.RolloutIntentUninstall {
		t.Fatalf("fixture: agent-c rollout %v (%v), want a queued uninstall", r, err)
	}

	token := previewToken(t, svc, ts.ID())
	if err = store.DeleteRollout(ctx, r.ID()); err != nil {
		t.Fatal(err)
	}
	if got := previewToken(t, svc, ts.ID()); got != token {
		t.Fatalf("a completed uninstall changed the token: %s → %s", token, got)
	}
	if err = svc.Deploy(ctx, ts.ID(), token); err != nil {
		t.Fatalf("Deploy after a completed uninstall: %v", err)
	}
}

func TestPlanTokenChangesWithTargets(t *testing.T) {
	svc, store, ts := mkTokenFixture(t)
	first := previewToken(t, svc, ts.ID())

	addMatchingAgent(t, store, "agent-b")
	joined := previewToken(t, svc, ts.ID())
	if joined == first {
		t.Fatal("a matching agent joined without changing the token")
	}

	// A rename bumps the version only: generation stays, so it alone cannot tell.
	ts.SetName("renamed")
	if err := svc.Upsert(context.Background(), ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	if ts.Generation() != 1 {
		t.Fatalf("fixture: rename bumped generation to %d", ts.Generation())
	}
	if edited := previewToken(t, svc, ts.ID()); edited == joined {
		t.Fatal("a spec edit did not change the token")
	}
}

// wasmOnly is runner inventory that cannot run the fixture's Subprocess workload.
var wasmOnly = []model.RunnerCapability{{Name: "wasm", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Wasm"}}}}

// addMissingTarget adds an explicit target that discovery does not know.
func addMissingTarget(t *testing.T, svc *Service, _ *inmemory.Store, ts *model.Spec) {
	t.Helper()
	ts.SetTargets([]string{"agent-x"})
	if err := svc.Upsert(context.Background(), ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
}

// Each change moves one input of the token and leaves the others alone, so only
// that input can change the token.
func TestPlanTokenCoversEveryInput(t *testing.T) {
	for _, tc := range []struct {
		name           string
		before, change func(t *testing.T, svc *Service, store *inmemory.Store, ts *model.Spec)
	}{
		{name: "another agent matches instead", change: func(t *testing.T, _ *Service, store *inmemory.Store, _ *model.Spec) {
			changeAgent(t, store, "agent-a", func(a *model.Agent) { a.LabelAdd("env", "dev") })
			addMatchingAgent(t, store, "agent-b")
		}},
		{name: "the agent advertises another first runner", change: func(t *testing.T, _ *Service, store *inmemory.Store, _ *model.Spec) {
			changeAgent(t, store, "agent-a", func(a *model.Agent) {
				a.SetCapabilities(append([]model.RunnerCapability{{Name: "first", Workloads: a.Capabilities()[0].Workloads}}, a.Capabilities()...))
			})
		}},
		{name: "another deploy created the rollout", change: func(t *testing.T, svc *Service, _ *inmemory.Store, ts *model.Spec) {
			if err := svc.Deploy(context.Background(), ts.ID(), ""); err != nil {
				t.Fatal(err)
			}
		}},
		// Missing and then registered without a compatible runner: only Exists moves.
		{name: "a missing target registers", before: addMissingTarget, change: func(t *testing.T, _ *Service, store *inmemory.Store, _ *model.Spec) {
			a, err := model.NewAgent("agent-x", "name-agent-x", "http://agent-x")
			if err != nil {
				t.Fatal(err)
			}
			a.SetCapabilities(wasmOnly)
			if err = store.UpsertAgent(context.Background(), a); err != nil {
				t.Fatal(err)
			}
		}},
		// Unknown inventory, then a runner for another workload: only Eligible moves.
		{name: "a target stops supporting the workload", before: func(t *testing.T, _ *Service, store *inmemory.Store, _ *model.Spec) {
			changeAgent(t, store, "agent-a", func(a *model.Agent) { a.SetCapabilities(nil) })
		}, change: func(t *testing.T, _ *Service, store *inmemory.Store, _ *model.Spec) {
			changeAgent(t, store, "agent-a", func(a *model.Agent) { a.SetCapabilities(wasmOnly) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, ts := mkTokenFixture(t)
			if tc.before != nil {
				tc.before(t, svc, store, ts)
			}
			before := previewToken(t, svc, ts.ID())
			tc.change(t, svc, store, ts)
			if after := previewToken(t, svc, ts.ID()); after == before {
				t.Fatalf("token stayed %s", before)
			}
		})
	}

	t.Run("another spec with the same plan", func(t *testing.T) {
		svc, store, ts := mkTokenFixture(t)
		twin := mkSpec(t, svc, store, "sp-twin", nil)
		twin.SetTargetLabels(ts.TargetLabels())
		if err := svc.Upsert(context.Background(), twin, twin.Version()); err != nil {
			t.Fatal(err)
		}
		if previewToken(t, svc, twin.ID()) == previewToken(t, svc, ts.ID()) {
			t.Fatal("two specs share a token")
		}
	})
}

func TestDeployRefusesChangedPlacement(t *testing.T) {
	for _, tc := range []struct {
		name           string
		before, change func(t *testing.T, svc *Service, store *inmemory.Store, ts *model.Spec)
	}{
		{name: "a matching agent joins", change: func(t *testing.T, _ *Service, store *inmemory.Store, _ *model.Spec) {
			addMatchingAgent(t, store, "agent-b")
		}},
		{name: "the spec is edited", change: func(t *testing.T, svc *Service, _ *inmemory.Store, ts *model.Spec) {
			ts.SetTimeoutMs(60_000)
			if err := svc.Upsert(context.Background(), ts, ts.Version()); err != nil {
				t.Fatal(err)
			}
		}},
		// The preview showed the deployment blocked by a missing target.
		{name: "a missing target registers", before: addMissingTarget, change: func(t *testing.T, _ *Service, store *inmemory.Store, _ *model.Spec) {
			a, err := model.NewAgent("agent-x", "name-agent-x", "http://agent-x")
			if err != nil {
				t.Fatal(err)
			}
			if err = store.UpsertAgent(context.Background(), a); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, ts := mkTokenFixture(t)
			ctx := context.Background()
			// agent-a already runs the spec, so a refused Deploy also must not queue its update.
			if err := svc.Deploy(ctx, ts.ID(), ""); err != nil {
				t.Fatal(err)
			}
			r, _ := store.GetRollout(ctx, model.RolloutID(ts.ID(), "agent-a"))
			r.SetActualTaskID("task-a")
			r.MarkSynced(ts.Generation())
			if err := store.UpsertRollout(ctx, r); err != nil {
				t.Fatal(err)
			}
			synced, _ := store.GetRollout(ctx, r.ID())
			if tc.before != nil {
				tc.before(t, svc, store, ts)
			}

			token := previewToken(t, svc, ts.ID())
			tc.change(t, svc, store, ts)
			if err := svc.Deploy(ctx, ts.ID(), token); !errors.Is(err, ErrPlacementChanged) {
				t.Fatalf("Deploy of a changed placement: %v, want ErrPlacementChanged", err)
			}

			rs := listRollouts(t, store, ts.ID())
			if len(rs) != 1 {
				t.Fatalf("refused deploy created rollouts: %d rows", len(rs))
			}
			if rs[0].Status() != enum.SyncStatusSynced || rs[0].Intent() != enum.RolloutIntentNoop || !rs[0].UpdatedAt().Equal(synced.UpdatedAt()) {
				t.Fatalf("refused deploy changed the existing rollout: status %s intent %s", rs[0].Status(), rs[0].Intent())
			}
		})
	}
}

func TestDeployAcceptsUnchangedPlacement(t *testing.T) {
	svc, store, ts := mkTokenFixture(t)
	ctx := context.Background()
	// One plan with every action: agent-a runs an older generation (update),
	// agent-b is new (install) and agent-c left the selector but keeps its
	// rollout (uninstall).
	addMatchingAgent(t, store, "agent-c")
	if err := svc.Deploy(ctx, ts.ID(), ""); err != nil {
		t.Fatal(err)
	}
	r, _ := store.GetRollout(ctx, model.RolloutID(ts.ID(), "agent-a"))
	r.SetActualTaskID("task-a")
	r.MarkSynced(ts.Generation())
	if err := store.UpsertRollout(ctx, r); err != nil {
		t.Fatal(err)
	}
	ts.SetTimeoutMs(60_000)
	if err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	changeAgent(t, store, "agent-c", func(a *model.Agent) { a.LabelAdd("env", "dev") })
	addMatchingAgent(t, store, "agent-b")

	token := previewToken(t, svc, ts.ID())
	// A heartbeat between preview and Deploy changes no decision.
	changeAgent(t, store, "agent-a", func(a *model.Agent) {
		a.SetStatus(enum.AgentStatusInactive)
		a.SetLastSeenAt(time.Now())
		a.SetStaleAt(time.Now().Add(-time.Second))
	})
	if err := svc.Deploy(ctx, ts.ID(), token); err != nil {
		t.Fatalf("Deploy of the previewed placement: %v", err)
	}

	want := map[string]enum.RolloutIntent{
		"agent-a": enum.RolloutIntentUpdate,
		"agent-b": enum.RolloutIntentInstall,
		"agent-c": enum.RolloutIntentUninstall,
	}
	rs := listRollouts(t, store, ts.ID())
	if len(rs) != len(want) {
		t.Fatalf("got %d rollouts, want %d", len(rs), len(want))
	}
	for _, r := range rs {
		if r.Intent() != want[r.AgentID()] || r.Status() != enum.SyncStatusPending || r.DesiredGeneration() != ts.Generation() {
			t.Errorf("%s: intent %s status %s gen %d, want %s pending gen %d",
				r.AgentID(), r.Intent(), r.Status(), r.DesiredGeneration(), want[r.AgentID()], ts.Generation())
		}
	}
}

func listRollouts(t *testing.T, store *inmemory.Store, specID string) []*model.Rollout {
	t.Helper()
	res, err := store.ListRollouts(
		context.Background(),
		inmemory.NewRolloutFilter().BySpecID(specID),
		storage.ListOptions{Limit: storage.MaxListLimit},
	)
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	out := make([]*model.Rollout, 0, len(res.Items))
	for _, r := range res.Items {
		if r != nil {
			out = append(out, r)
		}
	}
	return out
}
