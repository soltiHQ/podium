package httpserver

import "errors"

var (
	// ErrNilHandler indicates the http.Handler is nil.
	ErrNilHandler = errors.New("httpserver: nil handler")
	// ErrEmptyAddr indicates the listen address is empty.
	ErrEmptyAddr = errors.New("httpserver: empty addr")
	// ErrInvalidAddr indicates a listen address that is not host:port with a
	// numeric port.
	ErrInvalidAddr = errors.New("httpserver: addr is not host:port with a numeric port")
	// ErrNegativeValue indicates a negative timeout or size; zero keeps the
	// default.
	ErrNegativeValue = errors.New("httpserver: negative value")
	// ErrAlreadyStarted indicates Start was called more than once.
	ErrAlreadyStarted = errors.New("httpserver: already started")
)
