package kind

// TaskStatus describes the execution state of an agent task.
// Constants are ordered by display priority: active states first, terminal last.
type TaskStatus uint8

const (
	TaskStatusRunning   TaskStatus = iota // actively executing
	TaskStatusPending                     // queued, waiting for a slot
	TaskStatusFailed                      // exited with an error
	TaskStatusTimeout                     // exceeded deadline
	TaskStatusExhausted                   // all retry attempts used
	TaskStatusCanceled                    // explicitly canceled
	TaskStatusSucceeded                   // completed successfully
	TaskStatusUnknown                     // unrecognised status string
)

// Priority returns the sort weight (lower = more important).
func (s TaskStatus) Priority() int { return int(s) }

// String returns the lowercase status label.
func (s TaskStatus) String() string {
	switch s {
	case TaskStatusRunning:
		return "running"
	case TaskStatusPending:
		return "pending"
	case TaskStatusFailed:
		return "failed"
	case TaskStatusTimeout:
		return "timeout"
	case TaskStatusExhausted:
		return "exhausted"
	case TaskStatusCanceled:
		return "canceled"
	case TaskStatusSucceeded:
		return "succeeded"
	default:
		return "unknown"
	}
}

// ParseTaskStatus maps a status string to a typed constant.
func ParseTaskStatus(s string) TaskStatus {
	switch s {
	case "running":
		return TaskStatusRunning
	case "pending":
		return TaskStatusPending
	case "failed":
		return TaskStatusFailed
	case "timeout":
		return TaskStatusTimeout
	case "exhausted":
		return TaskStatusExhausted
	case "canceled":
		return TaskStatusCanceled
	case "succeeded":
		return TaskStatusSucceeded
	default:
		return TaskStatusUnknown
	}
}
