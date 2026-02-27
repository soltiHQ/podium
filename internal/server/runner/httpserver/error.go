package httpserver

import "errors"

var (
	// ErrNilHandler indicates the http.Handler is nil.
	ErrNilHandler = errors.New("httpserver: nil handler")
	// ErrEmptyAddr indicates the listen address is empty.
	ErrEmptyAddr = errors.New("httpserver: empty addr")
	// ErrAlreadyStarted indicates Start was called more than once.
	ErrAlreadyStarted = errors.New("httpserver: already started")
)
