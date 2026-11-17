package spec

import (
	"bytes"
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
	"github.com/soltiHQ/control-plane/internal/service/workload"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
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
	plan, err := buildPlacement(ts, []*model.Agent{a}, now, nil)
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

func TestPlacementUnknownInventoryBlocksLikeTheSchemaReasons(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "placement-block", []string{"quiet"})
	// mkSpec's agent reports no runners: nothing says it can run the workload.
	quiet, _ := store.GetAgent(ctx, "quiet")
	quiet.SetCapabilities(nil)
	if err := store.UpsertAgent(ctx, quiet); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil || plan.DeployAllowed || plan.BlockingReason != "incompatible_targets" || plan.Targets[0].Eligible || plan.Targets[0].InventoryKnown || plan.Targets[0].EligibilityReason != "inventory_unknown" {
		t.Fatalf("an agent without runners is not blocked: %+v %v", plan, err)
	}
	var incompatible *IncompatibleTargetsError
	if err := svc.Deploy(ctx, ts.ID(), nil); !errors.As(err, &incompatible) || !reflect.DeepEqual(incompatible.Reasons, map[string]string{"quiet": "inventory_unknown"}) {
		t.Fatalf("Deploy = %v", err)
	}
	if rollouts, _ := svc.RolloutsBySpec(ctx, ts.ID()); len(rollouts) != 0 {
		t.Fatal("a refused deploy wrote rollouts")
	}
	// Once it reports a runner, the same spec deploys to it.
	quiet.SetCapabilities([]model.RunnerCapability{containerRunner("r", testFingerprint(t))})
	if err := store.UpsertAgent(ctx, quiet); err != nil {
		t.Fatal(err)
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
	if _, err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
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
	if _, err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
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

func TestPlacementNeverMatchesAWorkloadlessSpecToMalformedInventory(t *testing.T) {
	ts, _ := model.NewSpec("invalid-kind", "invalid", "slot")
	ts.SetTargets([]string{"agent"})
	a, _ := model.NewAgent("agent", "agent", "http://agent")
	a.SetCapabilities([]model.RunnerCapability{{Name: "malformed", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: ""}}}})
	plan, err := buildPlacement(ts, []*model.Agent{a}, time.Now(), nil)
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

// Each runner fingerprint, the one the agent routes the workload to, gives the
// outcome its schema has for the body: the spec's own and another schema the
// body fits are compatible, one that refuses it is schema_incompatible with the
// problems, one podium lacks is pending and none is unpublished.
func TestPlacementComparesFingerprints(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "fingerprints", []string{"agent"})
	// The body {"image": "busybox"} is valid for permissiveSchema and not for portSchema.
	valid := seedSchema(t, store, permissiveSchema)
	invalid := seedSchema(t, store, portSchema)
	for _, tc := range []struct {
		name, fingerprint, reason string
	}{
		{"equal", testFingerprint(t), "compatible"},
		{"valid", valid, "compatible"},
		{"invalid", invalid, "schema_incompatible"},
		{"unknown", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "schema_pending"},
		{"unpublished", "", "schema_unpublished"},
	} {
		a, err := store.GetAgent(ctx, "agent")
		if err != nil {
			t.Fatal(err)
		}
		a.SetCapabilities([]model.RunnerCapability{containerRunner(tc.name, tc.fingerprint)})
		if err = store.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
		plan, err := svc.PreviewPlacement(ctx, ts.ID())
		if err != nil {
			t.Fatal(err)
		}
		target := plan.Targets[0]
		r := target.Runners[0]
		compatible := tc.reason == "compatible"
		if !r.WorkloadMatch || r.Reason != tc.reason || r.Eligible != compatible || target.Eligible != compatible {
			t.Fatalf("%s: runner %s eligible %v, target eligible %v; want %s", tc.name, r.Reason, r.Eligible, target.Eligible, tc.reason)
		}
		if compatible && (target.SelectedRunner != tc.name || target.EligibilityReason != "compatible_runner") {
			t.Fatalf("%s: selected %q (%s)", tc.name, target.SelectedRunner, target.EligibilityReason)
		}
		if tc.name == "invalid" && !strings.Contains(r.Detail, "port") {
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

// The agent routes a task to the first runner, in its declared order, whose
// workload type and labels match, whatever that runner's schema: placement
// evaluates that runner alone. An incompatible first runner makes the target
// ineligible, with its reason, even when a later runner would take the body;
// the later runners are not evaluated. Runners whose type or labels do not
// match are passed over, as the agent passes them over.
func TestPlacementEvaluatesTheRunnerTheAgentRoutesTo(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "first-runner", []string{"agent"})
	ts.SetRunnerLabels(map[string]string{"zone": "a"})
	if _, err := svc.Upsert(ctx, ts, ts.Version()); err != nil {
		t.Fatal(err)
	}
	refuses := seedSchema(t, store, portSchema)
	zoned := func(name, zone, fingerprint string) model.RunnerCapability {
		r := containerRunner(name, fingerprint)
		r.Labels = map[string]string{"zone": zone}
		return r
	}
	for _, tc := range []struct {
		name     string
		runners  []model.RunnerCapability
		eligible bool
		reason   string // the target's
		selected string
		reasons  []string // each runner's, in order
	}{
		{
			name:    "incompatible first",
			runners: []model.RunnerCapability{zoned("refuses", "a", refuses), zoned("fits", "a", testFingerprint(t))},
			reason:  "schema_incompatible", reasons: []string{"schema_incompatible", ""},
		},
		{
			name:     "compatible first",
			runners:  []model.RunnerCapability{zoned("fits", "a", testFingerprint(t)), zoned("refuses", "a", refuses)},
			eligible: true, reason: "compatible_runner", selected: "fits", reasons: []string{"compatible", ""},
		},
		{
			name: "other types and labels passed over",
			runners: []model.RunnerCapability{
				{Name: "wasm", Labels: map[string]string{"zone": "a"}, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Wasm"}}},
				zoned("other-zone", "b", testFingerprint(t)),
				zoned("refuses", "a", refuses),
				zoned("fits", "a", testFingerprint(t)),
			},
			reason: "schema_incompatible", reasons: []string{"workload_unsupported", "runner_labels_mismatch", "schema_incompatible", ""},
		},
		{
			name:    "pending first",
			runners: []model.RunnerCapability{zoned("pending", "a", strings.Repeat("f", 64)), zoned("fits", "a", testFingerprint(t))},
			reason:  "schema_pending", reasons: []string{"schema_pending", ""},
		},
		{
			name:    "unpublished first",
			runners: []model.RunnerCapability{zoned("unpublished", "a", ""), zoned("fits", "a", testFingerprint(t))},
			reason:  "schema_unpublished", reasons: []string{"schema_unpublished", ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := store.GetAgent(ctx, "agent")
			if err != nil {
				t.Fatal(err)
			}
			a.SetCapabilities(tc.runners)
			if err = store.UpsertAgent(ctx, a); err != nil {
				t.Fatal(err)
			}
			plan, err := svc.PreviewPlacement(ctx, ts.ID())
			if err != nil {
				t.Fatal(err)
			}
			target := plan.Targets[0]
			if target.Eligible != tc.eligible || target.EligibilityReason != tc.reason || target.SelectedRunner != tc.selected || plan.DeployAllowed != tc.eligible {
				t.Fatalf("target eligible %v (%s), selected %q, deploy allowed %v; want %v (%s), selected %q",
					target.Eligible, target.EligibilityReason, target.SelectedRunner, plan.DeployAllowed, tc.eligible, tc.reason, tc.selected)
			}
			var reasons []string
			for _, r := range target.Runners {
				reasons = append(reasons, r.Reason)
				if r.Eligible && r.Name != tc.selected {
					t.Fatalf("runner %s is eligible, but the agent routes to %q", r.Name, tc.selected)
				}
			}
			if !reflect.DeepEqual(reasons, tc.reasons) {
				t.Fatalf("runner reasons %q, want %q", reasons, tc.reasons)
			}
			wantNames := []string{}
			if tc.selected != "" {
				wantNames = []string{tc.selected}
			}
			if !reflect.DeepEqual(target.RunnerNames, wantNames) {
				t.Fatalf("runner names %q, want %q", target.RunnerNames, wantNames)
			}
			var incompatible *IncompatibleTargetsError
			switch err := svc.Deploy(ctx, ts.ID(), nil); {
			case tc.eligible && err != nil:
				t.Fatalf("Deploy = %v", err)
			case !tc.eligible && (!errors.As(err, &incompatible) || incompatible.Reasons["agent"] != tc.reason):
				t.Fatalf("Deploy = %v, want it refused with %s", err, tc.reason)
			}
		})
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

// flakySource wraps the catalog, fails to load the fingerprints in broken, and
// counts the fingerprints it is asked to load.
type flakySource struct {
	SchemaSource
	broken map[string]bool
	loaded map[string]int
}

func (f flakySource) Compiled(ctx context.Context, fingerprint string) (*workloadschema.Schema, bool, error) {
	f.loaded[fingerprint]++
	if f.broken[fingerprint] {
		return nil, false, errors.New("stored schema no longer compiles")
	}
	return f.SchemaSource.Compiled(ctx, fingerprint)
}

// A schema that cannot be loaded marks the runner the agent routes the workload
// to schema_incompatible; it does not fail the plan. The schemas of the other
// runners, of the type or not, are not even looked at.
func TestPlacementSurvivesASchemaThatCannotBeLoaded(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "broken", []string{"agent"})
	brokenOwn := seedSchema(t, store, portSchema)
	brokenOther := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	source := flakySource{SchemaSource: svc.schemas, broken: map[string]bool{brokenOwn: true, brokenOther: true}, loaded: map[string]int{}}
	svc.schemas = source
	otherType := model.RunnerCapability{Name: "other-type", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Unrelated", SchemaFingerprint: brokenOther}}}
	a, _ := store.GetAgent(ctx, "agent")
	a.SetCapabilities([]model.RunnerCapability{otherType, containerRunner("fine", testFingerprint(t)), containerRunner("own-broken", brokenOwn)})
	if err := store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil {
		t.Fatalf("the plan failed on a broken schema: %v", err)
	}
	runners := plan.Targets[0].Runners
	if runners[0].Reason != "workload_unsupported" || runners[0].Detail != "" || runners[1].Reason != "compatible" || runners[2].Reason != "" {
		t.Fatalf("runners: %+v", runners)
	}
	if err = svc.Deploy(ctx, ts.ID(), nil); err != nil {
		t.Fatalf("a broken schema of another runner blocked the deploy: %v", err)
	}
	if len(source.loaded) != 0 {
		t.Fatalf("schemas of runners the agent does not route to were loaded: %v", source.loaded)
	}

	// The agent now routes the workload to the runner whose schema cannot be loaded.
	a.SetCapabilities([]model.RunnerCapability{containerRunner("own-broken", brokenOwn), containerRunner("fine", testFingerprint(t))})
	if err = store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if plan, err = svc.PreviewPlacement(ctx, ts.ID()); err != nil {
		t.Fatalf("the plan failed on a broken schema: %v", err)
	}
	if r := plan.Targets[0].Runners[0]; r.Reason != "schema_incompatible" || r.Detail != "the schema could not be loaded" || plan.Targets[0].Eligible {
		t.Fatalf("own-broken: %+v", r)
	}
	var incompatible *IncompatibleTargetsError
	if err = svc.Deploy(ctx, ts.ID(), nil); !errors.As(err, &incompatible) {
		t.Fatalf("Deploy = %v, want the broken schema to block it", err)
	}
}

func TestOutcomeOfValidationMapsTooExpensive(t *testing.T) {
	for name, err := range map[string]error{
		"bare":    workloadschema.ErrValidationTooExpensive,
		"wrapped": fmt.Errorf("validating: %w", workloadschema.ErrValidationTooExpensive),
	} {
		got := outcomeOfValidation(err)
		if got.reason != "schema_incompatible" || !strings.Contains(got.detail, "too costly to check") {
			t.Errorf("%s: %+v", name, got)
		}
	}
	if got := outcomeOfValidation(nil); got.reason != "compatible" {
		t.Errorf("a valid body: %+v", got)
	}
	if got := outcomeOfValidation(errors.New("boom")); got.reason != "schema_incompatible" || got.detail != "boom" {
		t.Errorf("another error: %+v", got)
	}
}

// Deploy validates before its transaction. A workload changed in between is not
// the one the outcomes were computed for: the plan is refused, nothing is written.
func TestDeployRefusesAWorkloadChangedAfterThePrefetch(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "changed", []string{"agent"})
	pre, err := svc.prefetch(ctx, ts.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = loadPlacement(ctx, store, nil, zerolog.Nop(), ts, time.Now(), pre); err != nil {
		t.Fatalf("an unchanged workload: %v", err)
	}
	changed := ts.Clone()
	w := changed.Workload()
	w.Body = map[string]any{"image": "other"}
	changed.SetWorkload(w)
	if _, _, err = loadPlacement(ctx, store, nil, zerolog.Nop(), changed, time.Now(), pre); !errors.Is(err, ErrPlacementChanged) {
		t.Fatalf("a workload changed after the prefetch: %v", err)
	}
	// The runner labels pick the runner each agent routes the workload to: the
	// outcomes were computed for the labels read before the transaction.
	relabelled := ts.Clone()
	relabelled.SetRunnerLabels(map[string]string{"zone": "b"})
	if _, _, err = loadPlacement(ctx, store, nil, zerolog.Nop(), relabelled, time.Now(), pre); !errors.Is(err, ErrPlacementChanged) {
		t.Fatalf("runner labels changed after the prefetch: %v", err)
	}
}

// A running target whose runner now publishes a schema the body does not fit
// blocks the next deploy, and the deploy leaves its rollout as it was.
func TestBlockedDeployLeavesTheRolloutOfARunningTarget(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, store, "running", []string{"agent"})
	if err := svc.Deploy(ctx, ts.ID(), nil); err != nil {
		t.Fatal(err)
	}
	before, _ := svc.RolloutsBySpec(ctx, ts.ID())
	a, _ := store.GetAgent(ctx, "agent")
	a.SetCapabilities([]model.RunnerCapability{containerRunner("runner", seedSchema(t, store, portSchema))})
	if err := store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	var incompatible *IncompatibleTargetsError
	if err := svc.Deploy(ctx, ts.ID(), nil); !errors.As(err, &incompatible) || incompatible.Reasons["agent"] != "schema_incompatible" {
		t.Fatalf("Deploy = %v", err)
	}
	after, _ := svc.RolloutsBySpec(ctx, ts.ID())
	if len(after) != 1 || len(before) != 1 || after[0].Intent() != before[0].Intent() || !after[0].UpdatedAt().Equal(before[0].UpdatedAt()) || after[0].Status() != before[0].Status() {
		t.Fatal("a blocked deploy changed the rollout of a running target")
	}
}

// brokenSchema has a fingerprint of its own, and Compile refuses it.
const brokenSchema = `{"type":"nonsense"}`

// A schema podium refused, at the exchange or when it compiled one stored by
// another version, makes its runner schema_incompatible, with the reason as the
// detail, instead of a schema_pending that never ends.
func TestPlacementShowsARefusedSchema(t *testing.T) {
	ctx := context.Background()
	brokenFP, err := workloadschema.Fingerprint([]byte(brokenSchema))
	if err != nil {
		t.Fatal(err)
	}
	for name, refuse := range map[string]func(t *testing.T, catalog *workload.Service, store storage.Storage){
		"offered": func(t *testing.T, catalog *workload.Service, _ storage.Storage) {
			declared := []model.RunnerCapability{containerRunner("r", brokenFP)}
			catalog.Exchange(ctx, "agent", declared, []workload.OfferedSchema{{Fingerprint: brokenFP, Schema: []byte(brokenSchema)}})
		},
		"stored": func(t *testing.T, _ *workload.Service, store storage.Storage) {
			entry, err := model.NewWorkloadSchema(brokenFP, testAPIVersion, testKind, []byte(brokenSchema), "older-podium", time.Unix(1700000000, 0))
			if err != nil {
				t.Fatal(err)
			}
			if err = store.UpsertWorkloadSchema(ctx, entry); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, store := mkService(t)
			catalog := workload.New(store, zerolog.Nop())
			svc.schemas = catalog
			ts := mkSpec(t, svc, store, "refused", []string{"agent"})
			refuse(t, catalog, store)
			a, _ := store.GetAgent(ctx, "agent")
			a.SetCapabilities([]model.RunnerCapability{containerRunner("r", brokenFP)})
			if err := store.UpsertAgent(ctx, a); err != nil {
				t.Fatal(err)
			}
			plan, err := svc.PreviewPlacement(ctx, ts.ID())
			if err != nil {
				t.Fatal(err)
			}
			target := plan.Targets[0]
			r := target.Runners[0]
			if target.Eligible || target.EligibilityReason != "schema_incompatible" || r.Reason != "schema_incompatible" ||
				!strings.HasPrefix(r.Detail, "podium refused this schema: ") || !strings.Contains(r.Detail, "nonsense") {
				t.Fatalf("target %s, runner %s %q; want schema_incompatible, detail naming the refusal", target.EligibilityReason, r.Reason, r.Detail)
			}
		})
	}
}

// A store error behind a schema shows as a generic detail: the error itself,
// which may say more than a reader of the placement should see, goes to the log
// with the fingerprint.
func TestPlacementHidesAStoreErrorBehindAGenericDetail(t *testing.T) {
	svc, store := mkService(t)
	ctx := context.Background()
	logs := &bytes.Buffer{}
	svc.logger = zerolog.New(logs)
	ts := mkSpec(t, svc, store, "store-error", []string{"agent"})
	broken := seedSchema(t, store, portSchema)
	svc.schemas = failingSource{SchemaSource: svc.schemas, fail: map[string]error{broken: errors.New("pq: connection to 10.0.0.7 refused")}}
	a, _ := store.GetAgent(ctx, "agent")
	a.SetCapabilities([]model.RunnerCapability{containerRunner("r", broken)})
	if err := store.UpsertAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PreviewPlacement(ctx, ts.ID())
	if err != nil {
		t.Fatal(err)
	}
	if r := plan.Targets[0].Runners[0]; r.Reason != "schema_incompatible" || r.Detail != "the schema could not be loaded" {
		t.Fatalf("runner %s %q, want schema_incompatible with the generic detail", r.Reason, r.Detail)
	}
	if !strings.Contains(logs.String(), "10.0.0.7") || !strings.Contains(logs.String(), broken) {
		t.Fatalf("the log does not hold the error and the fingerprint: %s", logs.String())
	}
}

// failingSource wraps the catalog and fails to load the fingerprints in fail,
// each with its error.
type failingSource struct {
	SchemaSource
	fail map[string]error
}

func (f failingSource) Compiled(ctx context.Context, fingerprint string) (*workloadschema.Schema, bool, error) {
	if err, ok := f.fail[fingerprint]; ok {
		return nil, false, err
	}
	return f.SchemaSource.Compiled(ctx, fingerprint)
}
