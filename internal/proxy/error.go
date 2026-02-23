package proxy

import "errors"

var (
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
	// ErrListTasks indicates a gRPC ListTasks call failed.
	ErrListTasks = errors.New("proxy: grpc list tasks")
	// ErrUnsupportedAPIVersion indicates the agent reported an unknown API version.
	ErrUnsupportedAPIVersion = errors.New("proxy: unsupported api version")
)
