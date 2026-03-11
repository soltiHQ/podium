package sync

import "errors"

var (
	// ErrAlreadyStarted indicates Start was called more than once.
	ErrAlreadyStarted = errors.New("sync: already started")
)
