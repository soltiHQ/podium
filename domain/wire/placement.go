package wire

import (
	"time"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

func PlacementToREST(plan *model.PlacementPlan) restv1.SpecPlacement {
	if plan == nil {
		return restv1.SpecPlacement{}
	}
	out := restv1.SpecPlacement{
		SpecID: plan.SpecID, SpecVersion: plan.SpecVersion, Generation: plan.Generation,
		CheckedAt: plan.CheckedAt.Format(time.RFC3339Nano), DeployAllowed: plan.DeployAllowed,
		NoTargets: plan.NoTargets, DeletionRequested: plan.DeletionRequested, BlockingReason: plan.BlockingReason,
		Targets: make([]restv1.PlacementTarget, 0, len(plan.Targets)),
	}
	for _, t := range plan.Targets {
		x := restv1.PlacementTarget{
			AgentID: t.AgentID, AgentName: t.AgentName, Exists: t.Exists, Status: t.Status,
			HeartbeatFreshness: t.HeartbeatFreshness, MatchedBy: append([]string{}, t.MatchedBy...), InventoryKnown: t.InventoryKnown,
			RunnerNames: append([]string{}, t.RunnerNames...), SelectedRunner: t.SelectedRunner,
			Eligible: t.Eligible, EligibilityReason: t.EligibilityReason, Runners: make([]restv1.PlacementRunner, 0, len(t.Runners)),
		}
		if !t.LastSeenAt.IsZero() {
			x.LastSeenAt = t.LastSeenAt.Format(time.RFC3339Nano)
		}
		if !t.HeartbeatDueAt.IsZero() {
			x.HeartbeatDueAt = t.HeartbeatDueAt.Format(time.RFC3339Nano)
		}
		for _, r := range t.Runners {
			labels := make(map[string]string, len(r.Labels))
			for k, v := range r.Labels {
				labels[k] = v
			}
			x.Runners = append(x.Runners, restv1.PlacementRunner{
				Name: r.Name, Labels: labels, WorkloadMatch: r.WorkloadMatch,
				LabelsMatch: r.LabelsMatch, Eligible: r.Eligible, Reason: r.Reason,
			})
		}
		out.Targets = append(out.Targets, x)
	}
	return out
}
