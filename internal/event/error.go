package event

import "errors"

var (
	// ErrNilHub indicates a nil *Hub was provided to a constructor.
	ErrNilHub = errors.New("event: nil hub")
	// ErrSSENotSupported indicates the ResponseWriter does not support streaming.
	ErrSSENotSupported = errors.New("event: streaming not supported")
	// ErrSSEDeadline indicates the write deadline could not be disabled for SSE.
	ErrSSEDeadline = errors.New("event: cannot disable write deadline")
)
