package spec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	//__DEL__
	"sort"
	//__DEL__
	"time"

	//__DEL__

	"github.com/soltiHQ/control-plane/domain/model"
	//__DEL__
	"github.com/soltiHQ/control-plane/internal/storage"
	// __DEL__
)

// PreviewPlacement evaluates a saved specification without mutating storage or
// contacting agents. Deploy computes the same plan inside its write transaction.
// Inventory can change after preview: Deploy refuses the plan's Token once it no
// longer matches. CheckedAt identifies the observation.
func (s *Service) PreviewPlacement(ctx context.Context, specID string) (*model.PlacementPlan, error) {
	if specID == "" {
		return nil, storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ts, err := s.store.GetSpec(ctx, specID)
	if err != nil {
		return nil, err
	}
	plan, _, err := loadPlacement(ctx, s.store, ts, time.Now())
	return plan, err
}

// CheckTarget evaluates agent a as a target of spec ts, as a placement does:
// the runner the agent routes the workload to (see [model.PlacementTarget]) and
// its schema fingerprint. It writes nothing and contacts no agent. A nil agent
// is missing_agent. The sync runner checks each target with it before every push.
func CheckTarget(ts *model.Spec, a *model.Agent, now time.Time) model.PlacementTarget {
	cmp := &schemaCompat{workload: ts.Workload()}
	t := planTarget(a, ts, now, cmp)
	if a != nil {
		t.AgentID = a.ID()
	}
	return t
}

// loadPlacement reads the inventory and the spec's rollouts, and returns the
// plan with its token and the steps Deploy executes for it. Preview and Deploy
// both call it, so the same stored state gives them the same token.
func loadPlacement(ctx context.Context, store storage.Storage, ts *model.Spec, now time.Time) (*model.PlacementPlan, []deployStep, error) {
	agents, err := placementAgents(ctx, store, ts)
	if err != nil {
		return nil, nil, err
	}
	rollouts, err := storage.AllRollouts(ctx, store, store.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: ts.ID()}))
	if err != nil {
		return nil, nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, err
	}
	plan, err := buildPlacement(ts, agents, now)
	if err != nil {
		return nil, nil, err
	}
	steps := deploySteps(plan, rollouts)
	plan.Token = planToken(plan, steps)
	return plan, steps, nil
}

// placementAgents reads the agents a spec's placement considers: all of them
// when it selects by label, else the explicit targets that exist.
func placementAgents(ctx context.Context, store storage.Storage, ts *model.Spec) ([]*model.Agent, error) {
	var agents []*model.Agent
	if len(ts.TargetLabels()) > 0 {
		var err error
		agents, err = storage.AllAgents(ctx, store, nil)
		if err != nil {
			return nil, err
		}
	} else {
		seen := make(map[string]bool)
		for _, id := range ts.Targets() {
			if seen[id] {
				continue
			}
			seen[id] = true
			a, err := store.GetAgent(ctx, id)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			agents = append(agents, a)
		}
	}
	return agents, nil
}

// deployAction is what Deploy does with one agent's rollout of the spec.
type deployAction string

const (
	// deployInstall creates the rollout of a target that has none.
	deployInstall deployAction = "install"
	// deployUpdate brings a target's rollout to the spec generation. It writes
	// nothing when the rollout is already synced at that generation.
	deployUpdate deployAction = "update"
	// deployUninstall queues the removal of a rollout whose agent is no longer a target.
	deployUninstall deployAction = "uninstall"
)

// deployStep is one agent of a plan: whether Deploy targets it, whether a target
// exists in discovery and is eligible, the runner the plan selects for it and
// the action on its rollout (nil for an install).
type deployStep struct {
	agentID  string
	targeted bool
	exists   bool
	eligible bool
	runner   string
	action   deployAction
	rollout  *model.Rollout
}

// deploySteps maps a plan onto the spec's existing rollouts, sorted by agent ID.
// Deploy executes these steps and the plan token covers their targets, so
// preview and Deploy derive each agent's action from the same rollout rows in
// one place. An action depends only on whether a rollout exists: sync progress
// (status, task ID, observed generation) changes neither the action nor the token.
func deploySteps(plan *model.PlacementPlan, rollouts []*model.Rollout) []deployStep {
	existing := make(map[string]*model.Rollout, len(rollouts))
	for _, r := range rollouts {
		if r != nil {
			existing[r.AgentID()] = r
		}
	}
	steps := make([]deployStep, 0, len(plan.Targets)+len(existing))
	for _, target := range plan.Targets {
		step := deployStep{
			agentID: target.AgentID, targeted: true, exists: target.Exists, eligible: target.Eligible,
			runner: target.SelectedRunner, action: deployInstall,
		}
		if r, ok := existing[target.AgentID]; ok {
			step.action, step.rollout = deployUpdate, r
			delete(existing, target.AgentID)
		}
		steps = append(steps, step)
	}
	for agentID, r := range existing {
		steps = append(steps, deployStep{agentID: agentID, action: deployUninstall, rollout: r})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].agentID < steps[j].agentID })
	return steps
}

// planToken is the lowercase hex of the first 16 bytes of SHA-256 over the spec
// ID, version and generation and, for each target in agent ID order, the agent
// ID, whether it exists and is eligible, the runner and the action. Strings are
// length prefixed, so two different plans never share an encoding.
//
// Steps that only uninstall are left out: the target set already changes when
// an agent leaves the plan, and the sync runner deleting such a rollout once its
// uninstall completes must not ask for a new review. CheckedAt and the heartbeat
// observations (status, last seen, heartbeat due) are left out too: a heartbeat
// alone never changes the token.
func planToken(plan *model.PlacementPlan, steps []deployStep) string {
	b := appendTokenString(nil, plan.SpecID)
	b = binary.AppendVarint(b, int64(plan.SpecVersion))
	b = binary.AppendVarint(b, int64(plan.Generation))
	for _, step := range steps {
		if !step.targeted {
			continue
		}
		b = appendTokenString(b, step.agentID)
		var flags byte
		if step.exists {
			flags |= 1
		}
		if step.eligible {
			flags |= 2
		}
		b = append(b, flags)
		b = appendTokenString(b, step.runner)
		b = appendTokenString(b, string(step.action))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func appendTokenString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// buildPlacement is the single pure policy for preview and actual deployment.
// Explicit IDs are unioned with ALL-label matches. Empty labels match no extra
// agents. Runners retain registration order, not alphabetical order, and each
// target is evaluated on the runner its agent routes the workload to.
func buildPlacement(ts *model.Spec, agents []*model.Agent, now time.Time) (*model.PlacementPlan, error) {
	plan := &model.PlacementPlan{
		SpecID: ts.ID(), SpecVersion: ts.Version(), Generation: ts.Generation(), CheckedAt: now,
		DeployAllowed: !ts.DeletionRequested(), DeletionRequested: ts.DeletionRequested(), Targets: []model.PlacementTarget{},
	}
	if ts.DeletionRequested() {
		plan.BlockingReason = "spec_deleting"
	}
	inventory := make(map[string]*model.Agent, len(agents))
	for _, a := range agents {
		if a != nil {
			inventory[a.ID()] = a
		}
	}
	wanted := make(map[string][]string)
	for _, id := range ts.Targets() {
		if id == "" {
			return nil, storage.ErrInvalidArgument
		}
		wanted[id] = []string{"explicit"}
	}
	labels := ts.TargetLabels()
	if len(labels) > 0 {
		for id, a := range inventory {
			if agentMatchesLabels(a, labels) {
				wanted[id] = append(wanted[id], "labels")
			}
		}
	}
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	plan.NoTargets = len(ids) == 0
	cmp := &schemaCompat{workload: ts.Workload()}
	for _, id := range ids {
		target := planTarget(inventory[id], ts, now, cmp)
		target.AgentID = id
		target.MatchedBy = wanted[id]
		plan.Targets = append(plan.Targets, target)
		if !target.Eligible {
			plan.DeployAllowed = false
		}
	}
	if !ts.DeletionRequested() {
		switch b := blockedTargets(plan); {
		case len(b.missing) > 0:
			plan.BlockingReason = "unknown_targets"
		case len(b.unsupported) > 0:
			plan.BlockingReason = "unsupported_targets"
		case len(b.incompatible) > 0:
			plan.BlockingReason = "incompatible_targets"
		}
	}
	return plan, nil
}

func agentMatchesLabels(a *model.Agent, labels map[string]string) bool {
	for k, v := range labels {
		if actual, ok := a.Label(k); !ok || actual != v {
			return false
		}
	}
	return true
}

// compat is how a runner's schema relates to the spec's workload body.
type compat struct{ reason, detail string }

// schemaCompat compares a runner's schema fingerprint with the spec's.
type schemaCompat struct {
	workload model.Workload
}

// check gives the placement reason for a runner that publishes fingerprint:
//   - empty                          schema_unpublished
//   - the spec's own fingerprint     compatible
//   - another fingerprint            schema_incompatible
func (c *schemaCompat) check(fingerprint string) compat {
	if fingerprint == "" {
		return compat{reason: "schema_unpublished"}
	}
	if fingerprint == c.workload.SchemaFingerprint {
		return compat{reason: "compatible"}
	}
	return compat{reason: "schema_incompatible", detail: "the runner's schema fingerprint differs from the spec's"}
}

// routedRunner returns the index in runners of the runner an agent routes a
// task of type want to, as the SDK router does: the first, in declared order,
// that declares want's exact group, version and kind and whose labels match
// every one of selector, whatever its schema. It returns the fingerprint that
// runner declares for the type too, and -1 when no runner matches both.
func routedRunner(runners []model.RunnerCapability, want model.GroupVersionKind, selector map[string]string) (int, string) {
	for i, r := range runners {
		if fp, ok := declaredFingerprint(r, want); ok && labelsMatch(r.Labels, selector) {
			return i, fp
		}
	}
	return -1, ""
}

// declaredFingerprint reports whether runner declares want's exact group,
// version and kind, and the fingerprint it declares for the type.
func declaredFingerprint(runner model.RunnerCapability, want model.GroupVersionKind) (string, bool) {
	for _, gvk := range runner.Workloads {
		if gvk.Group == want.Group && gvk.Version == want.Version && gvk.Kind == want.Kind {
			return gvk.SchemaFingerprint, true
		}
	}
	return "", false
}

// labelsMatch reports whether labels hold every key of selector with its value.
func labelsMatch(labels, selector map[string]string) bool {
	for k, v := range selector {
		if actual, ok := labels[k]; !ok || actual != v {
			return false
		}
	}
	return true
}

// planTarget evaluates agent a as a target of ts. Of its runners only the one
// the agent routes the workload to is evaluated (see [model.PlacementTarget]):
// the target's eligibility and reason are that runner's.
func planTarget(a *model.Agent, ts *model.Spec, now time.Time, cmp *schemaCompat) model.PlacementTarget {
	t := model.PlacementTarget{Status: "unknown", HeartbeatFreshness: "unknown", RunnerNames: []string{}, Runners: []model.PlacementRunner{}}
	if a == nil {
		t.EligibilityReason = "missing_agent"
		return t
	}
	t.Exists = true
	t.AgentName = a.Name()
	t.Status = a.Status().String()
	t.LastSeenAt = a.LastSeenAt()
	t.HeartbeatDueAt = a.StaleAt()
	if !t.LastSeenAt.IsZero() && !t.HeartbeatDueAt.IsZero() {
		t.HeartbeatFreshness = "fresh"
		if now.After(t.HeartbeatDueAt) {
			t.HeartbeatFreshness = "overdue"
		}
	}
	capabilities := a.Capabilities()
	t.InventoryKnown = len(capabilities) > 0
	if !t.InventoryKnown {
		// An agent that has reported no runners cannot run the workload: nothing
		// says which schema it would check the body against.
		t.EligibilityReason = "inventory_unknown"
		return t
	}
	// The workload type is matched exactly. A spec without a workload has no
	// type, and matches no runner.
	want, wantErr := cmp.workload.GVK()
	selector := ts.RunnerLabels()
	routed := -1
	if wantErr == nil {
		routed, _ = routedRunner(capabilities, want, selector)
	}
	anyWorkload := false
	for i, capability := range capabilities {
		r := model.PlacementRunner{Name: capability.Name, Labels: capability.Labels, LabelsMatch: labelsMatch(capability.Labels, selector)}
		var fingerprint string
		if wantErr == nil {
			fingerprint, r.WorkloadMatch = declaredFingerprint(capability, want)
		}
		anyWorkload = anyWorkload || r.WorkloadMatch
		switch {
		case !r.WorkloadMatch:
			r.Reason = "workload_unsupported"
		case !r.LabelsMatch:
			r.Reason = "runner_labels_mismatch"
		case i == routed:
			out := cmp.check(fingerprint)
			r.Reason, r.Detail, r.Eligible = out.reason, out.detail, out.reason == "compatible"
		}
		// A runner after the routed one whose type and labels match is not
		// evaluated: the agent never routes the workload to it.
		t.Runners = append(t.Runners, r)
	}
	switch {
	case routed >= 0 && t.Runners[routed].Eligible:
		t.Eligible = true
		t.SelectedRunner = t.Runners[routed].Name
		t.RunnerNames = append(t.RunnerNames, t.SelectedRunner)
		t.EligibilityReason = "compatible_runner"
	case routed >= 0:
		t.EligibilityReason = t.Runners[routed].Reason
	case anyWorkload:
		t.EligibilityReason = "runner_labels_mismatch"
	default:
		t.EligibilityReason = "workload_unsupported"
	}
	return t
}

// blocked is the targets that stop a deploy.
type blocked struct {
	// missing are agents absent from discovery.
	missing []string
	// unsupported are agents with no runner for the workload type or its labels.
	unsupported []string
	// incompatible are agents whose runners cannot take the body: no inventory,
	// or no schema the body fits. reasons gives each one's placement reason.
	incompatible []string
	reasons      map[string]string
}

// blockedTargets sorts the targets of a plan that are not eligible.
func blockedTargets(plan *model.PlacementPlan) blocked {
	b := blocked{reasons: map[string]string{}}
	for _, target := range plan.Targets {
		switch {
		case !target.Exists:
			b.missing = append(b.missing, target.AgentID)
		case target.Eligible:
		case isIncompatibleReason(target.EligibilityReason):
			b.incompatible = append(b.incompatible, target.AgentID)
			b.reasons[target.AgentID] = target.EligibilityReason
		default:
			b.unsupported = append(b.unsupported, target.AgentID)
		}
	}
	return b
}

func isIncompatibleReason(reason string) bool {
	switch reason {
	case "inventory_unknown", "schema_incompatible", "schema_pending", "schema_unpublished":
		return true
	}
	return false
}
