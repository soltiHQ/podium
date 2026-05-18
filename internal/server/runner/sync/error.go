package sync

import "errors"

// ErrAlreadyStarted indicates Start was called more than once.
var ErrAlreadyStarted = errors.New("sync: already started")
