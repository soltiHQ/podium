package restv1

// ExecutionRecord is an observed terminal attempt without manifests or logs.
type ExecutionRecord struct {
	ID                    string `json:"id"`
	AgentID               string `json:"agent_id"`
	AgentName             string `json:"agent_name"`
	TaskName              string `json:"task_name"`
	TaskUID               string `json:"task_uid"`
	Generation            uint64 `json:"generation"`
	Attempt               uint32 `json:"attempt"`
	Status                string `json:"status"`
	StartedAtMs           int64  `json:"started_at_ms"`
	FinishedAtMs          int64  `json:"finished_at_ms"`
	ExitCode              *int32 `json:"exit_code"`
	Error                 string `json:"error,omitempty"`
	WorkloadAPIVersion    string `json:"workload_api_version"`
	WorkloadKind          string `json:"workload_kind"`
	SpecID                string `json:"spec_id,omitempty"`
	SpecName              string `json:"spec_name,omitempty"`
	SourceResourceVersion string `json:"source_resource_version"`
	CapturedAt            string `json:"captured_at"`
}

type ListHistoryResponse struct {
	Items           []ExecutionRecord `json:"items"`
	NextCursor      string            `json:"next_cursor,omitempty"`
	Source          string            `json:"source"`
	CaptureComplete bool              `json:"capture_complete"`
}

// HistoryQuery is the exact-match query shared by the page and REST endpoint.
// Changing filters starts a new page; cursor is an opaque archive continuation.
type HistoryQuery struct {
	AgentID, SpecID, TaskName, TaskUID, Status, Cursor string
	Limit                                              int
}
