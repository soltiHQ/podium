package restv1

// Spec is the REST representation of a task specification.
type Spec struct {
	KindConfig   map[string]any    `json:"kind_config,omitempty"`
	TargetLabels map[string]string `json:"target_labels,omitempty"`
	RunnerLabels map[string]string `json:"runner_labels,omitempty"`
	CreateSpec   map[string]any    `json:"create_spec,omitempty"`
	Targets      []string          `json:"targets,omitempty"`

	BackoffFactor float64 `json:"backoff_factor"`

	TimeoutMs      int64 `json:"timeout_ms"`
	IntervalMs     int64 `json:"interval_ms,omitempty"`
	BackoffFirstMs int64 `json:"backoff_first_ms"`
	BackoffMaxMs   int64 `json:"backoff_max_ms"`

	Version int `json:"version"`

	ID          string `json:"id"`
	Name        string `json:"name"`
	Slot        string `json:"slot"`
	Jitter      string `json:"jitter"`
	KindType    string `json:"kind_type"`
	Admission   string `json:"admission"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	RestartType string `json:"restart_type"`
}

// SpecListResponse is the paginated list of specs.
type SpecListResponse struct {
	Items      []Spec `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// SpecCreateRequest is the request body for creating/updating a spec.
type SpecCreateRequest struct {
	TargetLabels map[string]string `json:"target_labels,omitempty"`
	RunnerLabels map[string]string `json:"runner_labels,omitempty"`
	KindConfig   map[string]any    `json:"kind_config,omitempty"`
	Targets      []string          `json:"targets,omitempty"`

	BackoffFactor float64 `json:"backoff_factor"`

	IntervalMs     int64 `json:"interval_ms,omitempty"`
	BackoffFirstMs int64 `json:"backoff_first_ms"`
	BackoffMaxMs   int64 `json:"backoff_max_ms"`
	TimeoutMs      int64 `json:"timeout_ms"`

	RestartType string `json:"restart_type"`
	KindType    string `json:"kind_type"`
	Admission   string `json:"admission"`
	Jitter      string `json:"jitter"`
	Name        string `json:"name"`
	Slot        string `json:"slot"`
}
