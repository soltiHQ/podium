package spec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
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

// loadPlacement reads the inventory and the spec's rollouts, and returns the
// plan with its token and the steps Deploy executes for it. Preview and Deploy
// both call it, so the same stored state gives them the same token.
func loadPlacement(ctx context.Context, store storage.Storage, ts *model.Spec, now time.Time) (*model.PlacementPlan, []deployStep, error) {
	var agents []*model.Agent
	if len(ts.TargetLabels()) > 0 {
		var err error
		agents, err = storage.AllAgents(ctx, store, nil)
		if err != nil {
			return nil, nil, err
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
				return nil, nil, err
			}
			agents = append(agents, a)
		}
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

// deployStep is one agent of a plan: whether Deploy targets it, the runner the
// plan selects for it and the action on its rollout (nil for an install).
type deployStep struct {
	agentID  string
	targeted bool
	runner   string
	action   deployAction
	rollout  *model.Rollout
}

// deploySteps maps a plan onto the spec's existing rollouts, sorted by agent ID.
// Deploy executes these steps and the plan token covers them, so preview and
// Deploy derive each agent's action from the same rollout rows in one place.
// An action depends only on whether a rollout exists: sync progress (status,
// task ID, observed generation) changes neither the action nor the token.
func deploySteps(plan *model.PlacementPlan, rollouts []*model.Rollout) []deployStep {
	existing := make(map[string]*model.Rollout, len(rollouts))
	for _, r := range rollouts {
		if r != nil {
			existing[r.AgentID()] = r
		}
	}
	steps := make([]deployStep, 0, len(plan.Targets)+len(existing))
	for _, target := range plan.Targets {
		step := deployStep{agentID: target.AgentID, targeted: true, runner: target.SelectedRunner, action: deployInstall}
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
// ID, version and generation and, for each step in agent ID order, the agent ID,
// whether Deploy targets it, the runner and the action. Strings are length
// prefixed, so two different plans never share an encoding. CheckedAt and the
// heartbeat observations (status, last seen, heartbeat due) are left out: a
// heartbeat alone never changes the token.
func planToken(plan *model.PlacementPlan, steps []deployStep) string {
	b := appendTokenString(nil, plan.SpecID)
	b = binary.AppendVarint(b, int64(plan.SpecVersion))
	b = binary.AppendVarint(b, int64(plan.Generation))
	for _, step := range steps {
		b = appendTokenString(b, step.agentID)
		targeted := byte(0)
		if step.targeted {
			targeted = 1
		}
		b = append(b, targeted)
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
// agents. Matching runners retain registration order, not alphabetical order.
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
	for _, id := range ids {
		target := planTarget(inventory[id], ts, now)
		target.AgentID = id
		target.MatchedBy = wanted[id]
		plan.Targets = append(plan.Targets, target)
		if !target.Eligible {
			plan.DeployAllowed = false
		}
	}
	if !ts.DeletionRequested() {
		if missing, _ := blockedTargets(plan); len(missing) > 0 {
			plan.BlockingReason = "unknown_targets"
		} else if _, unsupported := blockedTargets(plan); len(unsupported) > 0 {
			plan.BlockingReason = "unsupported_targets"
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

func planTarget(a *model.Agent, ts *model.Spec, now time.Time) model.PlacementTarget {
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
		t.Eligible = true
		t.EligibilityReason = "inventory_unknown"
		return t
	}
	kind := workloadKind(ts.KindType())
	anyWorkload := false
	selector := ts.RunnerLabels()
	for _, cap := range capabilities {
		r := model.PlacementRunner{Name: cap.Name, Labels: cap.Labels, LabelsMatch: true}
		for _, gvk := range cap.Workloads {
			if kind != "" && gvk.Group == "solti.io" && gvk.Version == "v1" && gvk.Kind == kind {
				r.WorkloadMatch = true
				anyWorkload = true
				break
			}
		}
		for k, v := range selector {
			if actual, ok := cap.Labels[k]; !ok || actual != v {
				r.LabelsMatch = false
				break
			}
		}
		r.Eligible = r.WorkloadMatch && r.LabelsMatch
		switch {
		case !r.WorkloadMatch:
			r.Reason = "workload_unsupported"
		case !r.LabelsMatch:
			r.Reason = "runner_labels_mismatch"
		default:
			r.Reason = "compatible"
		}
		t.Runners = append(t.Runners, r)
		if r.Eligible {
			t.RunnerNames = append(t.RunnerNames, r.Name)
		}
	}
	if len(t.RunnerNames) > 0 {
		t.Eligible = true
		t.SelectedRunner = t.RunnerNames[0]
		t.EligibilityReason = "compatible_runner"
	} else if anyWorkload {
		t.EligibilityReason = "runner_labels_mismatch"
	} else {
		t.EligibilityReason = "workload_unsupported"
	}
	return t
}

func workloadKind(kind enum.TaskKindType) string {
	switch kind {
	case enum.TaskKindSubprocess:
		return "Subprocess"
	case enum.TaskKindWasm:
		return "Wasm"
	case enum.TaskKindContainer:
		return "Container"
	default:
		return ""
	}
}

func blockedTargets(plan *model.PlacementPlan) (missing, unsupported []string) {
	for _, target := range plan.Targets {
		if !target.Exists {
			missing = append(missing, target.AgentID)
		} else if !target.Eligible {
			unsupported = append(unsupported, target.AgentID)
		}
	}
	return missing, unsupported
}
