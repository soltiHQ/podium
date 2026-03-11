package kind

// TaskKindType represents the execution backend for a task.
// https://github.com/soltiHQ/sdk/blob/main/crates/solti-model/src/kind/task.rs
type TaskKindType string

const (
	TaskKindSubprocess TaskKindType = "subprocess"
	TaskKindContainer  TaskKindType = "container"
	TaskKindWasm       TaskKindType = "wasm"
)
