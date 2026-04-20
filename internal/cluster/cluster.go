// Package cluster holds cluster-level abstractions used by the control plane
// to run as a single-node deployment or as a multi-replica HA cluster.
//
// Two interfaces live here:
//
//   - [Discovery]  — how replicas find one another
//   - [Leadership] — which replica runs singleton work
//
// Storage atomicity is part of [storage.TxStore.WithTx], not this package.
//
// Drivers live in sub-packages: internal/cluster/discovery,
// internal/cluster/standalone, internal/cluster/raft.
package cluster

import "context"

// Peer is a cluster member. ID is stable across reconnects; Address is a
// host:port the peer listens on for internal RPC.
type Peer struct {
	ID      string
	Address string
}

// Discovery resolves the set of cluster peers.
type Discovery interface {
	// Peers returns the current snapshot.
	Peers(ctx context.Context) ([]Peer, error)
	// Watch emits updated peer lists when they change; the channel closes
	// when ctx is cancelled. Drivers that cannot observe changes may emit
	// only the initial snapshot.
	Watch(ctx context.Context) <-chan []Peer
}

// Leadership provides singleton-execution guarantees for runners.
type Leadership interface {
	// AmLeader reports whether this replica is currently leader.
	AmLeader() bool
	// CurrentLeader returns the leader's address or "" if unknown.
	CurrentLeader() string
	// WhenLeader blocks until leader, then invokes fn with a context
	// cancelled on leadership loss. When leadership is regained fn is
	// called again. Returns when the outer ctx is cancelled.
	WhenLeader(ctx context.Context, fn func(context.Context) error) error
}
