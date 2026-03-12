package server

import "context"

// Runner is a managed runtime component.
//
// Start blocks until the component finishes or ctx is canceled.
// Stop performs graceful shutdown within the ctx deadline.
// Name returns a human-readable identifier for logging.
type Runner interface {
	Name() string
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}
