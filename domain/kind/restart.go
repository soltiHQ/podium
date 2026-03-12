package kind

// RestartType determines whether a task should be automatically restarted.
type RestartType string

const (
	RestartNever     RestartType = "never"
	RestartAlways    RestartType = "always"
	RestartOnFailure RestartType = "onFailure"
)
