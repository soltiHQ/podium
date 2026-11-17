package spec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
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
	plan, err := buildPlacement(context.Background(), ts, []*model.Agent{a}, now, nil)
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
	if err := svc.Deploy(ctx, ts.ID(), nil); err != nil {
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
	if err := svc.Deploy(ctx, ts.ID(), nil); !errors.As(err, &unsupported) {
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
	if err := svc.Deploy(ctx, ts.ID(), nil); !errors.As(err, &missing) {
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

func TestPlacementNeverMatchesAWorkloadlessSpecToMalformedInventory(t *testing.T) {
	ts, _ := model.NewSpec("invalid-kind", "invalid", "slot")
	ts.SetTargets([]string{"agent"})
	a, _ := model.NewAgent("agent", "agent", "http://agent")
	a.SetCapabilities([]model.RunnerCapability{{Name: "malformed", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: ""}}}})
	plan, err := buildPlacement(context.Background(), ts, []*model.Agent{a}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DeployAllowed || plan.Targets[0].EligibilityReason != "workload_unsupported" {
		t.Fatalf("a spec without a workload matched a malformed capability: %+v", plan)
	}
}

// A second schema of the Container type, one that accepts a body the first
// refuses and refuses a body the first accepts.
const (
	permissiveSchema = `{"type":"object"}`
	portSchema       = `{"type":"object","required":["image","port"],"properties":{"image":{"type":"string"},"port":{"type":"integer"}}}`
)

// seedSchema puts a schema of the test type into the catalog and returns its fingerprint.
func seedSchema(t testing.TB, store storage.Storage, schema string) string {
	t.Helper()
	fp, err := workloadschema.Fingerprint([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := model.NewWorkloadSchema(fp, testAPIVersion, testKind, []byte(schema), "agent-1", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.UpsertWorkloadSchema(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	return fp
}

func containerRunner(name, fingerprint string) model.RunnerCapability {
	return model.RunnerCapability{Name: name, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Container", SchemaFingerprint: fingerprint}}}
}

func TestPlacementComparesFingerprints(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "fingerprints", []string{"agent"})
	// The body {"image": "busybox"} is valid for permissiveSchema and not for portSchema.
	valid := seedSchema(t, store, permissiveSchema)
	invalid := seedSchema(t, store, portSchema)
	a, err := store.GetAgent(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	a.SetCapabilities([]model.RunnerCapability{
		containerRunner("equal", testFingerprint(t)),
		containerRunner("valid", valid),
		containerRunner("invalid", invalid),
		containerRunner("unknown", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
		containerRunner("unpublished", ""),
	})
	if err = store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil {
		t.Fatal(err)
	}
	target := plan.Targets[0]
	got := make(map[string]string)
	eligible := make(map[string]bool)
	for _, r := range target.Runners {
		got[r.Name], eligible[r.Name] = r.Reason, r.Eligible
		if !r.WorkloadMatch {
			t.Fatalf("runner %s does not match the workload type", r.Name)
		}
	}
	want := map[string]string{
		"equal": "compatible", "valid": "compatible", "invalid": "schema_incompatible",
		"unknown": "schema_pending", "unpublished": "schema_unpublished",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reasons = %v, want %v", got, want)
	}
	for name, reason := range want {
		if eligible[name] != (reason == "compatible") {
			t.Fatalf("runner %s: eligible = %v with reason %s", name, eligible[name], reason)
		}
	}
	if !reflect.DeepEqual(target.RunnerNames, []string{"equal", "valid"}) || target.SelectedRunner != "equal" || !target.Eligible || target.EligibilityReason != "compatible_runner" {
		t.Fatalf("only compatible runners are eligible: %+v", target)
	}
	for _, r := range target.Runners {
		if r.Name == "invalid" && !strings.Contains(r.Detail, "port") {
			t.Fatalf("the incompatible runner names no problem: %q", r.Detail)
		}
	}
}

func TestPlacementWithoutACompatibleRunnerExplainsWhy(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "reasons", []string{"agent"})
	invalid := seedSchema(t, store, portSchema)
	a, _ := store.GetAgent(ctx, "agent")
	for name, tc := range map[string]struct {
		runner model.RunnerCapability
		reason string
	}{
		"incompatible": {containerRunner("r", invalid), "schema_incompatible"},
		"pending":      {containerRunner("r", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"), "schema_pending"},
		"unpublished":  {containerRunner("r", ""), "schema_unpublished"},
	} {
		a.SetCapabilities([]model.RunnerCapability{tc.runner})
		if err := store.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
		plan, err := svc.PreviewPlacement(ctx, ts.ID())
		if err != nil {
			t.Fatal(err)
		}
		if target := plan.Targets[0]; target.Eligible || target.EligibilityReason != tc.reason || plan.DeployAllowed || plan.BlockingReason != "incompatible_targets" {
			t.Fatalf("%s: %+v / %q", name, target, plan.BlockingReason)
		}
	}
}

// Review Focus 3: one spec, two targets whose runners publish two schemas, and
// a body valid for one of them. The plan has one compatible and one
// schema_incompatible target, and Deploy refuses only the second.
func TestDeployRefusesOnlyIncompatibleTargets(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "focus-3", []string{"fits", "misfits"})
	for id, schema := range map[string]string{"fits": permissiveSchema, "misfits": portSchema} {
		fp := seedSchema(t, store, schema)
		a, err := store.GetAgent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		a.SetCapabilities([]model.RunnerCapability{containerRunner("runner", fp)})
		if err = store.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Targets) != 2 || plan.Targets[0].AgentID != "fits" || plan.Targets[1].AgentID != "misfits" {
		t.Fatalf("targets: %+v", plan.Targets)
	}
	if r := plan.Targets[0].Runners[0]; !plan.Targets[0].Eligible || r.Reason != "compatible" {
		t.Fatalf("fits: %+v", plan.Targets[0])
	}
	if r := plan.Targets[1].Runners[0]; plan.Targets[1].Eligible || r.Reason != "schema_incompatible" || plan.Targets[1].EligibilityReason != "schema_incompatible" {
		t.Fatalf("misfits: %+v", plan.Targets[1])
	}
	if plan.DeployAllowed || plan.BlockingReason != "incompatible_targets" {
		t.Fatalf("plan: allowed %v, blocking %q", plan.DeployAllowed, plan.BlockingReason)
	}
	var incompatible *IncompatibleTargetsError
	err = svc.Deploy(ctx, ts.ID(), nil)
	if !errors.As(err, &incompatible) {
		t.Fatalf("Deploy = %v, want an IncompatibleTargetsError", err)
	}
	if !reflect.DeepEqual(incompatible.Agents, []string{"misfits"}) || !reflect.DeepEqual(incompatible.Reasons, map[string]string{"misfits": "schema_incompatible"}) {
		t.Fatalf("refused %v with %v, want only misfits", incompatible.Agents, incompatible.Reasons)
	}
	var unsupported *UnsupportedTargetsError
	if errors.As(err, &unsupported) {
		t.Fatal("an incompatible target is reported as unsupported")
	}
	if rollouts, _ := svc.RolloutsBySpec(ctx, ts.ID()); len(rollouts) != 0 {
		t.Fatalf("a refused deploy wrote %d rollouts", len(rollouts))
	}

	// Once the second agent runs a schema the body fits, Deploy goes ahead.
	a, _ := store.GetAgent(ctx, "misfits")
	a.SetCapabilities([]model.RunnerCapability{containerRunner("runner", seedSchema(t, store, permissiveSchema))})
	if err = store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = svc.Deploy(ctx, ts.ID(), nil); err != nil {
		t.Fatalf("Deploy after the fix: %v", err)
	}
}
