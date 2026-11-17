package model

import (
	"time"
	"unicode/utf8"
)

// PlacementPlan describes the saved desired state against observed discovery
// inventory. It is an advisory plan, not a reservation or execution result.
type PlacementPlan struct {
	SpecID      string
	SpecVersion int
	Generation  int
	// Token identifies what Deploy would do with this plan: the spec revision
	// and, per target, whether it exists and is eligible, its selected runner
	// and the action on its rollout. CheckedAt and heartbeat observations do not
	// change it. Deploy refuses a token that no longer matches.
	Token             string
	CheckedAt         time.Time
	DeployAllowed     bool
	NoTargets         bool
	DeletionRequested bool
	BlockingReason    string
	Targets           []PlacementTarget
}

// PlacementTarget is one agent of a plan.
//
// The agent routes a task to the first runner, in declared order, whose
// workload type and labels match: the SDK router takes the first runner that
// declares the workload's exact type and matches the runner selector, whatever
// its schema. Placement evaluates that runner, and only that one: the target's
// eligibility and reason are that runner's, and SelectedRunner names it when it
// is compatible. A target whose first matching runner is not compatible is not
// eligible, even when a later runner would take the body, since the agent never
// routes the task to the later one. The runners after it are not evaluated:
// they have no reason and are not eligible. See EvaluatedRunner.
type PlacementTarget struct {
	AgentID            string
	AgentName          string
	Exists             bool
	Status             string
	LastSeenAt         time.Time
	HeartbeatDueAt     time.Time
	HeartbeatFreshness string
	MatchedBy          []string
	InventoryKnown     bool
	// RunnerNames names the eligible runners: the selected one, or none.
	RunnerNames       []string
	SelectedRunner    string
	Eligible          bool
	EligibilityReason string
	Runners           []PlacementRunner
}

// EvaluatedRunner returns the runner placement evaluated for the target: the
// first, in declared order, whose workload type and labels match, which is the
// one the agent routes the task to. It is false when no runner matches both.
func (t PlacementTarget) EvaluatedRunner() (PlacementRunner, bool) {
	for _, r := range t.Runners {
		if r.WorkloadMatch && r.LabelsMatch {
			return r, true
		}
	}
	return PlacementRunner{}, false
}

// PlacementRunner preserves advertised registration order, which is the SDK's
// selection order. Eligibility describes the workload type, the runner labels
// and the runner's schema (whether the body fits it), not backend health.
type PlacementRunner struct {
	Name          string
	Labels        map[string]string
	WorkloadMatch bool
	LabelsMatch   bool
	Eligible      bool
	// Reason is empty for a runner that matches the type and the labels after
	// the evaluated one: placement does not evaluate it.
	Reason string
	// Detail says more about a refusal: the first schema problems, that the body
	// was too expensive to validate, that podium refused the schema, or that it
	// could not be loaded. It is empty for other reasons. Where it leaves podium
	// it is cut to PlacementDetailMaxBytes (BoundPlacementDetail).
	Detail string
}

// PlacementDetailMaxBytes bounds the detail of a runner where it leaves podium:
// in a placement over REST, and in the error of a rollout whose push the sync
// runner refused.
const PlacementDetailMaxBytes = 1 << 10

// BoundPlacementDetail cuts a detail to PlacementDetailMaxBytes at a rune
// boundary.
func BoundPlacementDetail(s string) string {
	if len(s) <= PlacementDetailMaxBytes {
		return s
	}
	cut := PlacementDetailMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
