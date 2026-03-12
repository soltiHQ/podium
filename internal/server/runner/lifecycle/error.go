package lifecycle

import "errors"

var (
	// ErrAlreadyStarted indicates Start was called more than once.
	ErrAlreadyStarted = errors.New("lifecycle: already started")
)
