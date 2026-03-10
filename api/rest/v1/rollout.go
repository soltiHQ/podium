package restv1

// RolloutSpec embeds Spec with a per-agent delivery state.
type RolloutSpec struct {
	Spec
	Entries []RolloutEntry `json:"rollout,omitempty"`
}

// RolloutEntry tracks the delivery state of a spec on a single agent.
type RolloutEntry struct {
	DesiredVersion int `json:"desired_version"`
	ActualVersion  int `json:"actual_version"`
	Attempts       int `json:"attempts,omitempty"`

	LastPushedAt string `json:"last_pushed_at,omitempty"`
	LastSyncedAt string `json:"last_synced_at,omitempty"`
	AgentID      string `json:"agent_id"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}
