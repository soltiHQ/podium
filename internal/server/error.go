package server

import (
	"errors"
	"fmt"
)

var (
	// ErrDuplicateRunnerName indicates the Server was created with duplicate runner names.
	ErrDuplicateRunnerName = errors.New("server: duplicate runner name")
	// ErrEmptyRunnerName indicates the runner name is empty.
	ErrEmptyRunnerName = errors.New("server: runner has empty name")
	// ErrNoRunners indicates the Server was created without any runners.
	ErrNoRunners = errors.New("server: no runners configured")
	// ErrNilRunner indicates one of the provided runners is nil.
	ErrNilRunner = errors.New("server: nil runner")
)

// RunnerError wraps a runner-specific error with its name and phase.
type RunnerError struct {
	Runner string
	Phase  string
	Err    error
}

func (e *RunnerError) Error() string {
	return fmt.Sprintf("server: runner %s %s: %v", e.Runner, e.Phase, e.Err)
}

func (e *RunnerError) Unwrap() error { return e.Err }

// RunnerExitedError indicates a runner stopped without an explicit shutdown request.
type RunnerExitedError struct {
	Runner string
}

func (e *RunnerExitedError) Error() string {
	return fmt.Sprintf("server: runner %s exited unexpectedly", e.Runner)
}
