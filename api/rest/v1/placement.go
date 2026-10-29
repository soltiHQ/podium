package restv1

// SpecPlacement is the current saved-spec routing plan. Deploy re-evaluates the
// inventory; accepted desired state does not prove reconciliation or execution.
type SpecPlacement struct {
	SpecID            string            `json:"spec_id"`
	SpecVersion       int               `json:"spec_version"`
	Generation        int               `json:"generation"`
	CheckedAt         string            `json:"checked_at"`
	DeployAllowed     bool              `json:"deploy_allowed"`
	NoTargets         bool              `json:"no_targets"`
	DeletionRequested bool              `json:"deletion_requested"`
	BlockingReason    string            `json:"blocking_reason,omitempty"`
	Targets           []PlacementTarget `json:"targets"`
}

type PlacementTarget struct {
	AgentID            string            `json:"agent_id"`
	AgentName          string            `json:"agent_name"`
	Exists             bool              `json:"exists"`
	Status             string            `json:"status"`
	LastSeenAt         string            `json:"last_seen_at,omitempty"`
	HeartbeatDueAt     string            `json:"heartbeat_due_at,omitempty"`
	HeartbeatFreshness string            `json:"heartbeat_freshness"`
	MatchedBy          []string          `json:"matched_by"`
	InventoryKnown     bool              `json:"inventory_known"`
	RunnerNames        []string          `json:"runner_names"`
	SelectedRunner     string            `json:"selected_runner,omitempty"`
	Eligible           bool              `json:"eligible"`
	EligibilityReason  string            `json:"eligibility_reason"`
	Runners            []PlacementRunner `json:"runners"`
}

type PlacementRunner struct {
	Name          string            `json:"name"`
	Labels        map[string]string `json:"labels"`
	WorkloadMatch bool              `json:"workload_match"`
	LabelsMatch   bool              `json:"labels_match"`
	Eligible      bool              `json:"eligible"`
	Reason        string            `json:"reason"`
}
