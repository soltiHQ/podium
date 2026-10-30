package model

import "time"

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
	RunnerNames        []string
	SelectedRunner     string
	Eligible           bool
	EligibilityReason  string
	Runners            []PlacementRunner
}

// PlacementRunner preserves advertised registration order, which is the SDK's
// selection order. Eligibility describes workload/labels, not backend health.
type PlacementRunner struct {
	Name          string
	Labels        map[string]string
	WorkloadMatch bool
	LabelsMatch   bool
	Eligible      bool
	Reason        string
}
