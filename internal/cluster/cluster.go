// Package cluster holds cluster-level abstractions used by the control plane
// to run as a single-node deployment or as a multi-replica HA cluster.
//
// Two interfaces live here:
//
//   - [Discovery]  — how replicas find one another
//   - [Leadership] — which replica runs singleton work
//
// Raft also implements [Membership], the members of its configuration, and
// [Admin], through which the cluster service changes them (admin.go).
//
// [SharedSettings] are the settings every replica must share; their [Digests]
// and [Fingerprint] (fingerprint.go) tell whether two nodes share them.
//
// Storage atomicity is part of [storage.TxStore.WithTx], not this package.
//
// Drivers live in sub-packages: internal/cluster/discovery,
// internal/cluster/standalone, internal/cluster/raft.
package cluster

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// ErrNotLeader indicates a write that reached a replica which is not the
// leader. The write changed nothing; the leader, when one is known, is
// [Leadership.CurrentLeader]. The transports answer it as unavailable (HTTP 503,
// gRPC Unavailable), and HTTP adds the leader's address. It lives here, apart
// from the raft package that returns it (as raft.ErrNotLeader), so that the
// transports can tell it without importing raft: errkind, which classifies it,
// cannot, as raft depends on errkind through domain/wire and the proxy.
var ErrNotLeader = errors.New("this replica is not the leader")

// ErrLeadershipLost indicates a write that was in flight when the replica lost
// leadership. Unlike [ErrNotLeader] it may still commit: the new leader can have
// the write in its log, so a client that retries it must be able to repeat it.
// The transports answer it as unavailable, and HTTP adds the leader's address when
// one is known.
var ErrLeadershipLost = errors.New("this replica lost leadership while the write was committing")

// IsLeadershipError reports whether err says that a write met the wrong
// leadership, [ErrNotLeader] or [ErrLeadershipLost]. That is a routing condition,
// not a failure of this replica: the transports answer it with the leader's
// address, and log it as a warning.
func IsLeadershipError(err error) bool {
	return errors.Is(err, ErrNotLeader) || errors.Is(err, ErrLeadershipLost)
}

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
	// called again. Returns when the outer ctx is cancelled, or with fn's
	// error. How often fn runs differs between drivers: raft calls fn again
	// whenever it returns while the node still leads, after another barrier,
	// so fn must return only once its context ends, or it runs again and
	// again; standalone, which always leads, calls fn once and returns what
	// it returns.
	WhenLeader(ctx context.Context, fn func(context.Context) error) error
}

// LeaderReadiness is implemented by drivers whose election can precede state
// replay. A successful WaitReady means leader reads include prior committed
// operations. Leadership alone must not be used as a replay readiness check.
type LeaderReadiness interface {
	WaitReady(ctx context.Context) error
}

// Membership is implemented by drivers whose leadership knows the members of
// its cluster: raft, from its configuration. Standalone has none.
type Membership interface {
	// Members returns the members of the latest configuration, committed or
	// not, each with the address the others reach it at, as written. Every
	// call reads the configuration anew, cheaply.
	Members() []Peer
}

// LeaderAddr is the address, host:port, at which the leader whose raft address
// is leaderAddr serves a listener that every replica serves on port (the
// same-port assumption): the host of the raft address, as written, with port.
// It is the one derivation of that address: the target a follower forwards a
// write to, the HTTP X-Leader header and the gRPC x-leader hint alike. It is
// empty without a leader or a port, since the address cannot be derived.
func LeaderAddr(leaderAddr string, port int) string {
	if leaderAddr == "" || port == 0 {
		return ""
	}
	host := leaderAddr
	if i := strings.LastIndex(leaderAddr, ":"); i > 0 {
		host = leaderAddr[:i]
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

// AwaitLeaderReady establishes the read-ready boundary before leader work.
// Drivers without asynchronous recovery retain the Leadership-only contract.
func AwaitLeaderReady(ctx context.Context, leader Leadership) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ready, ok := leader.(LeaderReadiness); ok {
		return ready.WaitReady(ctx)
	}
	return nil
}
