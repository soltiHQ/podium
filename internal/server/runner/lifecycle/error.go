package lifecycle

import "errors"

// ErrAlreadyStarted indicates Start was called more than once.
var ErrAlreadyStarted = errors.New("lifecycle: already started")
