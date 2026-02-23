package proxyv1

// Task represents a single task running on an agent.
type Task struct {
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
	Attempt   int   `json:"attempt"`

	ID     string `json:"id"`
	Slot   string `json:"slot"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// TaskListResponse is the response for listing agent tasks.
type TaskListResponse struct {
	Tasks []Task `json:"tasks"`
	Total int    `json:"total"`
}
