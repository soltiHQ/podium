package proxyv1

// TaskCondition preserves one agent controller condition. Status uses the SDK
// strings True, False and Unknown; it never substitutes for execution phase.
// LastTransitionTime is Unix milliseconds; nil preserves missing HTTP data.
type TaskCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	ObservedGeneration uint64 `json:"observed_generation"`
	LastTransitionTime *int64 `json:"last_transition_time"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
}

// Task is the current observed state of one task on an agent. Generation is
// desired state; ObservedGeneration is processed state, including failed
// reconciliation. Conditions retain that outcome independently of execution.
type Task struct {
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
	Attempt   int   `json:"attempt"`

	UID                string `json:"uid"`
	Generation         uint64 `json:"generation"`
	ObservedGeneration uint64 `json:"observed_generation"`
	ID                 string `json:"id"`
	Slot               string `json:"slot"`
	Status             string `json:"status"`
	Error              string `json:"error,omitempty"`

	ResourceVersion string          `json:"resource_version,omitempty"`
	ExitCode        *int32          `json:"exit_code,omitempty"`
	Conditions      []TaskCondition `json:"conditions"`
}

// ListTasksResponse is one cursor snapshot page of observed agent tasks.
type ListTasksResponse struct {
	Tasks           []Task  `json:"tasks"`
	Total           int     `json:"total"`
	NextCursor      string  `json:"next_cursor,omitempty"`
	ResourceVersion string  `json:"resource_version,omitempty"`
	RemainingCount  *uint64 `json:"remaining_count,omitempty"`
}

// TaskRun represents a single execution attempt of a task.
type TaskRun struct {
	WorkloadAPIVersion string `json:"workload_api_version,omitempty"`
	WorkloadKind       string `json:"workload_kind,omitempty"`
	FinishedAtPresent  bool   `json:"-"`
	Generation         uint64 `json:"generation"`
	StartedAt          int64  `json:"started_at"`
	FinishedAt         int64  `json:"finished_at,omitempty"`
	Attempt            int    `json:"attempt"`

	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	ExitCode *int32 `json:"exit_code,omitempty"`
}

// ListTaskRunsResponse is a task's full run-attempt history (bounded; not paginated).
type ListTaskRunsResponse struct {
	Runs    []TaskRun `json:"runs"`
	TaskUID string    `json:"task_uid,omitempty"`
}

// GetTaskResponse is the response for getting a single task by id.
type GetTaskResponse struct {
	Task *Task `json:"task,omitempty"`
}
