package proxy

import "errors"

var (
	ErrClosed = errors.New("proxy: pool closed")
	// ErrUnsupportedEndpointType indicates the agent reported an unknown endpoint type.
	ErrUnsupportedEndpointType = errors.New("proxy: unsupported endpoint type")
	// ErrDial indicates a failure to establish a gRPC connection to an agent.
	ErrDial = errors.New("proxy: grpc dial")
	// ErrClose indicates a failure to close a pooled gRPC connection.
	ErrClose = errors.New("proxy: grpc close")
	// ErrBadEndpointURL indicates the agent's endpoint is not a valid URL.
	ErrBadEndpointURL = errors.New("proxy: bad endpoint URL")
	// ErrCreateRequest indicates a failure to build the outbound HTTP request.
	ErrCreateRequest = errors.New("proxy: create request")
	// ErrRequest indicates the HTTP call to the agent failed (network, timeout, etc.).
	ErrRequest = errors.New("proxy: request to agent")
	// ErrUnexpectedStatus indicates the agent returned a non-200 HTTP status.
	ErrUnexpectedStatus = errors.New("proxy: unexpected status")
	// ErrDecode indicates the agent's response body could not be decoded.
	ErrDecode = errors.New("proxy: decode response")
	// ErrResponseTooLarge indicates the proxy refused the agent's answer for its
	// size: a body over the 4 MiB it reads from an HTTP agent (an ErrDecode too),
	// or a message over the receive limit of the gRPC client. A smaller request
	// may succeed, a list page with a lower limit for one. An agent's own
	// ResourceExhausted, a quota for one, does not carry it.
	ErrResponseTooLarge = errors.New("proxy: response too large")
	// ErrTooManyItems indicates a ListTasks or ListTaskRuns page that holds more
	// items than the request's limit, its page size for runs. The proxy refuses it
	// before it builds them, and the error is a decode failure too: no agent
	// answers so.
	ErrTooManyItems = errors.New("proxy: too many items")
	// ErrTooManyElements indicates an answer, or an item of one, that holds more
	// repeated elements than the proxy builds: more than a gRPC answer may hold in
	// all, or more conditions than a task may. It is an ErrTooManyItems, and a decode
	// failure too.
	ErrTooManyElements error = elementsError{}
	// ErrCursorTooLong indicates a ListTasks page whose next cursor is longer than
	// MaxCursorBytes. It is a decode failure too.
	ErrCursorTooLong = errors.New("proxy: cursor too long")
	// ErrListTasks indicates a ListTasks call failed (HTTP or gRPC).
	ErrListTasks = errors.New("proxy: list tasks")
	// ErrUnsupportedAPIVersion indicates the agent reported an unknown API version.
	ErrUnsupportedAPIVersion = errors.New("proxy: unsupported api version")
	// ErrApplyTask indicates an apply (supersede-or-install) call failed.
	ErrApplyTask = errors.New("proxy: apply task")
	// ErrGetTask indicates a GetTask call failed.
	ErrGetTask = errors.New("proxy: get task")
	// ErrListTaskRuns indicates a ListTaskRuns call failed.
	ErrListTaskRuns = errors.New("proxy: list task runs")
	// ErrDeleteTask indicates a DeleteTask call failed.
	ErrDeleteTask = errors.New("proxy: delete task")
	// ErrStreamTaskLogs indicates a StreamTaskLogs call failed before any
	// events arrived. Errors mid-stream are surfaced by closing the channel.
	ErrStreamTaskLogs = errors.New("proxy: stream task logs")
	// ErrNilPool indicates a required *Pool dependency is nil.
	ErrNilPool = errors.New("proxy: nil pool")
)

// elementsError is the type of ErrTooManyElements: it reads as its own error and is
// also ErrTooManyItems.
type elementsError struct{}

func (elementsError) Error() string { return "proxy: too many elements" }

func (elementsError) Unwrap() error { return ErrTooManyItems }
