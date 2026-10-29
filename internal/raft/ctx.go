package raft

import "context"

// contextBackground isolates the background ctx call for the (rare) places
// that need one without propagating a request ctx. Kept as a named function
// so grep-audits can find every use.
func contextBackground() context.Context { return context.Background() }
