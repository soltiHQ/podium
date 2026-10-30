package event

import "errors"

var (
	// ErrNilHub indicates a nil *Hub was provided to a constructor.
	ErrNilHub = errors.New("event: nil hub")
	// ErrSSENotSupported indicates the ResponseWriter does not support streaming.
	ErrSSENotSupported = errors.New("event: streaming not supported")
	// ErrSSEDeadline indicates the write deadline of an SSE connection could not be set.
	ErrSSEDeadline = errors.New("event: cannot set write deadline")
	// ErrNilAccess indicates a nil access check was given to SSEHandler.
	ErrNilAccess = errors.New("event: nil access check")
)
