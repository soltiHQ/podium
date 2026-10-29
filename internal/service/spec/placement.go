package spec

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// PreviewPlacement evaluates a saved specification without mutating storage or
// contacting agents. Deploy computes the same plan inside its write transaction.
// Inventory can change after preview; CheckedAt/version identify its observation.
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
	return loadPlacement(ctx, s.store, ts, time.Now())
}

func loadPlacement(ctx context.Context, store storage.Storage, ts *model.Spec, now time.Time) (*model.PlacementPlan, error) {
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return buildPlacement(ts, agents, now)
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
