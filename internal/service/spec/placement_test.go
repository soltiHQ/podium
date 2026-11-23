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
	if _, err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 503; i++ {
		a, _ := model.NewAgent(fmt.Sprintf("match-%04d", i), "matching", "http://matching")
		a.LabelAdd("env", "prod")
		a.LabelAdd("team", "ops")
		a.SetCapabilities([]model.RunnerCapability{containerRunner("runner", testFingerprint(t))})
		if err := store.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	partial, _ := model.NewAgent("partial", "partial", "http://partial")
	partial.LabelAdd("env", "prod")
	partial.SetCapabilities([]model.RunnerCapability{containerRunner("runner", testFingerprint(t))})
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
	if err := svc.Deploy(ctx, ts.ID(), nil); err != nil {
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
	ts.SetWorkload(testWorkload(t))
	ts.SetTargets([]string{"agent", "agent"})
	ts.SetTargetLabels(map[string]string{"env": "prod"})
	ts.SetRunnerLabels(map[string]string{"zone": "a"})
	a, _ := model.NewAgent("agent", "agent name", "http://agent")
	a.LabelAdd("env", "prod")
	a.SetStatus(enum.AgentStatusInactive)
	a.SetLastSeenAt(now.Add(-time.Minute))
	a.SetStaleAt(now.Add(-30 * time.Second))
	sub := []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Container", SchemaFingerprint: testFingerprint(t)}}
	a.SetCapabilities([]model.RunnerCapability{
		{Name: "other-workload", Labels: map[string]string{"zone": "a"}, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Subprocess"}}},
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
	if !reflect.DeepEqual(target.RunnerNames, []string{"z-first"}) || !reflect.DeepEqual(target.MatchedBy, []string{"explicit", "labels"}) {
		t.Fatalf("ordered runner/union metadata lost: %+v", target)
	}
	if target.Status != "inactive" || target.HeartbeatFreshness != "overdue" || !target.LastSeenAt.Equal(a.LastSeenAt()) {
		t.Fatalf("observed liveness lost: %+v", target)
	}
	if target.Runners[0].Reason != "workload_unsupported" || target.Runners[1].Reason != "runner_labels_mismatch" || target.Runners[2].Reason != "compatible" {
		t.Fatalf("routing explanations wrong: %+v", target.Runners)
	}
	// The agent routes the workload to z-first: a-second, which matches too, is
	// not evaluated.
	if r := target.Runners[3]; r.Reason != "" || r.Eligible || !r.WorkloadMatch || !r.LabelsMatch {
		t.Fatalf("a runner after the routed one was evaluated: %+v", r)
	}
}

func TestPlacementReevaluatesMetadataAndEmptyTargetsDrain(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "placement-drift", nil)
	ts.SetTargetLabels(map[string]string{"env": "prod"})
	if _, err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	a, _ := model.NewAgent("changing", "changing", "http://changing")
	a.LabelAdd("env", "prod")
	a.SetCapabilities([]model.RunnerCapability{containerRunner("runner", testFingerprint(t))})
	if err := store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	first, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil || len(first.Targets) != 1 {
		t.Fatalf("initial match: %+v %v", first, err)
	}
	if err := svc.Deploy(ctx, ts.ID(), nil); err != nil {
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
	if err := svc.Deploy(ctx, ts.ID(), nil); err != nil {
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

func containerRunner(name, fingerprint string) model.RunnerCapability {
	return model.RunnerCapability{Name: name, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Container", SchemaFingerprint: fingerprint}}}
}
