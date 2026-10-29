package spec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestPlacementMatchesDeployBeyond500AndUsesAllLabelUnion(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "placement-many", []string{"explicit", "explicit"})
	ts.SetTargetLabels(map[string]string{"env": "prod", "team": "ops"})
	if err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 503; i++ {
		a, _ := model.NewAgent(fmt.Sprintf("match-%04d", i), "matching", "http://matching")
		a.LabelAdd("env", "prod")
		a.LabelAdd("team", "ops")
		if err := store.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	partial, _ := model.NewAgent("partial", "partial", "http://partial")
	partial.LabelAdd("env", "prod")
	if err := store.UpsertAgent(ctx, partial); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DeployAllowed || plan.NoTargets || len(plan.Targets) != 504 {
		t.Fatalf("placement truncated or selected partial: %+v / %d", plan, len(plan.Targets))
	}
	if plan.SpecVersion != ts.Version() || plan.Generation != ts.Generation() || plan.CheckedAt.IsZero() {
		t.Fatal("plan lost its observed spec revision")
	}
	if got, _ := svc.RolloutsBySpec(ctx, ts.ID()); len(got) != 0 {
		t.Fatal("preview mutated desired rollouts")
	}
	if err := svc.Deploy(ctx, ts.ID()); err != nil {
		t.Fatal(err)
	}
	rollouts, _ := svc.RolloutsBySpec(ctx, ts.ID())
	got := make(map[string]bool, len(rollouts))
	for _, r := range rollouts {
		got[r.AgentID()] = true
	}
	for _, target := range plan.Targets {
		if !got[target.AgentID] {
			t.Fatalf("preview/deploy drifted at %s", target.AgentID)
		}
	}
	if len(got) != len(plan.Targets) {
		t.Fatal("deploy added an unplanned target")
	}
}

func TestPlacementPreservesRunnerRegistrationOrderAndObservedLiveness(t *testing.T) {
	now := time.Unix(1700000000, 0)
	ts, _ := model.NewSpec("plan", "plan", "slot")
	ts.SetTargets([]string{"agent", "agent"})
	ts.SetTargetLabels(map[string]string{"env": "prod"})
	ts.SetRunnerLabels(map[string]string{"zone": "a"})
	a, _ := model.NewAgent("agent", "agent name", "http://agent")
	a.LabelAdd("env", "prod")
	a.SetStatus(enum.AgentStatusInactive)
	a.SetLastSeenAt(now.Add(-time.Minute))
	a.SetStaleAt(now.Add(-30 * time.Second))
	sub := []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Subprocess"}}
	a.SetCapabilities([]model.RunnerCapability{
		{Name: "other-workload", Labels: map[string]string{"zone": "a"}, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Container"}}},
		{Name: "wrong-zone", Labels: map[string]string{"zone": "b"}, Workloads: sub},
		{Name: "z-first", Labels: map[string]string{"zone": "a"}, Workloads: sub},
		{Name: "a-second", Labels: map[string]string{"zone": "a"}, Workloads: sub},
	})
	plan, err := buildPlacement(ts, []*model.Agent{a}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := plan.Targets[0]
	if !plan.DeployAllowed || !target.Eligible || target.SelectedRunner != "z-first" {
		t.Fatalf("routing changed admission/order: %+v", target)
	}
	if !reflect.DeepEqual(target.RunnerNames, []string{"z-first", "a-second"}) || !reflect.DeepEqual(target.MatchedBy, []string{"explicit", "labels"}) {
		t.Fatalf("ordered runner/union metadata lost: %+v", target)
	}
	if target.Status != "inactive" || target.HeartbeatFreshness != "overdue" || !target.LastSeenAt.Equal(a.LastSeenAt()) {
		t.Fatalf("observed liveness lost: %+v", target)
	}
	if target.Runners[0].Reason != "workload_unsupported" || target.Runners[1].Reason != "runner_labels_mismatch" || target.Runners[2].Reason != "compatible" {
		t.Fatalf("routing explanations wrong: %+v", target.Runners)
	}
}

func TestPlacementUnknownInventoryAcceptedAndKnownIncompatibleBlocksAll(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "placement-block", []string{"unknown-inventory"})
	plan, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil || !plan.DeployAllowed || !plan.Targets[0].Eligible || plan.Targets[0].InventoryKnown || plan.Targets[0].EligibilityReason != "inventory_unknown" {
		t.Fatalf("missing inventory falsely rejected or claims runner: %+v %v", plan, err)
	}
	if err := svc.Deploy(ctx, ts.ID()); err != nil {
		t.Fatal(err)
	}
	before, _ := svc.RolloutsBySpec(ctx, ts.ID())
	blocked, _ := model.NewAgent("blocked", "blocked", "http://blocked")
	blocked.SetCapabilities([]model.RunnerCapability{{Name: "wasm", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Wasm"}}}})
	if err := store.UpsertAgent(ctx, blocked); err != nil {
		t.Fatal(err)
	}
	ts.SetTargets([]string{"blocked"})
	if err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	plan, err = svc.PreviewPlacement(ctx, ts.ID())
	if err != nil || plan.DeployAllowed || plan.BlockingReason != "unsupported_targets" || plan.Targets[0].EligibilityReason != "workload_unsupported" {
		t.Fatalf("incompatible inventory not blocked: %+v %v", plan, err)
	}
	var unsupported *UnsupportedTargetsError
	if err := svc.Deploy(ctx, ts.ID()); !errors.As(err, &unsupported) {
		t.Fatalf("preview/deploy admission drift: %v", err)
	}
	after, _ := svc.RolloutsBySpec(ctx, ts.ID())
	if len(after) != 1 || after[0].AgentID() != before[0].AgentID() || after[0].Intent() != before[0].Intent() || !after[0].UpdatedAt().Equal(before[0].UpdatedAt()) {
		t.Fatal("blocked whole deployment changed an existing rollout")
	}
	ts.SetTargets([]string{"missing"})
	if err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	plan, err = svc.PreviewPlacement(ctx, ts.ID())
	if err != nil || plan.DeployAllowed || plan.BlockingReason != "unknown_targets" || plan.Targets[0].Exists {
		t.Fatalf("unknown agent omitted/accepted: %+v %v", plan, err)
	}
	var missing *UnknownTargetsError
	if err := svc.Deploy(ctx, ts.ID()); !errors.As(err, &missing) {
		t.Fatalf("unknown admission drift: %v", err)
	}
}

func TestPlacementReevaluatesMetadataAndEmptyTargetsDrain(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "placement-drift", nil)
	ts.SetTargetLabels(map[string]string{"env": "prod"})
	if err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	a, _ := model.NewAgent("changing", "changing", "http://changing")
	a.LabelAdd("env", "prod")
	if err := store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	first, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil || len(first.Targets) != 1 {
		t.Fatalf("initial match: %+v %v", first, err)
	}
	if err := svc.Deploy(ctx, ts.ID()); err != nil {
		t.Fatal(err)
	}
	a.LabelAdd("env", "dev")
	if err := store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	second, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil || !second.DeployAllowed || !second.NoTargets || len(second.Targets) != 0 {
		t.Fatalf("preview cached stale inventory: %+v %v", second, err)
	}
	if err := svc.Deploy(ctx, ts.ID()); err != nil {
		t.Fatal(err)
	}
	rollouts, _ := svc.RolloutsBySpec(ctx, ts.ID())
	if len(rollouts) != 1 || rollouts[0].Intent() != enum.RolloutIntentUninstall {
		t.Fatal("empty placement/deployment policy drifted")
	}
}

type placementReadOnlyStore struct{ storage.Storage }

func (s placementReadOnlyStore) WithTx(context.Context, func(storage.Storage) error) error {
	return errors.New("preview attempted a write transaction")
}

func TestPlacementPreviewUsesOnlyReadsAndDeletionIsBlocked(t *testing.T) {
	svc, store := mkService(t)
	ts := mkSpec(t, svc, store, "placement-readonly", []string{"agent"})
	readOnly := New(placementReadOnlyStore{store}, zerolog.Nop())
	plan, err := readOnly.PreviewPlacement(context.Background(), ts.ID())
	if err != nil || !plan.DeployAllowed {
		t.Fatalf("read-only preview required mutation: %+v %v", plan, err)
	}
	ts.MarkForDeletion()
	if err := store.UpsertSpec(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
	plan, err = readOnly.PreviewPlacement(context.Background(), ts.ID())
	if err != nil || plan.DeployAllowed || !plan.DeletionRequested || plan.BlockingReason != "spec_deleting" {
		t.Fatalf("deleting spec accepted: %+v %v", plan, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readOnly.PreviewPlacement(ctx, ts.ID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preview: %v", err)
	}
}

func TestPlacementNeverMatchesUnknownKindToMalformedInventory(t *testing.T) {
	ts, _ := model.NewSpec("invalid-kind", "invalid", "slot")
	ts.SetKindType(enum.TaskKindType("unknown"))
	ts.SetTargets([]string{"agent"})
	a, _ := model.NewAgent("agent", "agent", "http://agent")
	a.SetCapabilities([]model.RunnerCapability{{Name: "malformed", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: ""}}}})
	plan, err := buildPlacement(ts, []*model.Agent{a}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if plan.DeployAllowed || plan.Targets[0].EligibilityReason != "workload_unsupported" {
		t.Fatalf("unknown kind matched malformed capability: %+v", plan)
	}
}
