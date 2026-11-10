package server

import "context"

// Runner is a managed runtime component.
//
// Start runs the component and blocks until it ends. The listener runners
// (httpserver, grpcserver) ignore ctx: they return after Stop, or when serving
// fails, and at once with nil, without binding, when Stop came first. The other
// runners return when ctx is canceled or Stop is called.
// Stop performs graceful shutdown within the ctx deadline. Server.Run cancels
// ctx before it calls Stop, and may call Stop before Start has run, so a Start
// that comes after Stop must not start serving.
// Name returns a human-readable identifier for logging.
type Runner interface {
	Name() string
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}
