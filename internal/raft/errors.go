package raft

import (
	"errors"
	"strings"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// ErrNotLeader is [cluster.ErrNotLeader]: what every write of a Store returns on
// a node that is not the leader, wrapped, so that errors.Is finds it. The write
// changed nothing, and the transports answer it as unavailable, HTTP with the
// leader's address.
var ErrNotLeader = cluster.ErrNotLeader

// clusterError gives an error of hashicorp/raft the meaning it has for the
// callers of a Store, who get an error that errkind classifies as Unavailable
// and that is still the hashicorp/raft error for errors.Is:
//
//   - ErrNotLeader is [cluster.ErrNotLeader]: this node is not the leader, and
//     the write changed nothing.
//   - ErrLeadershipTransferInProgress is [cluster.ErrNotLeader] as well: a
//     leader refuses every write, barrier and configuration change while it
//     hands its leadership over (leaderLoop in v1.8.0), so the write changed
//     nothing, and the next leader can take it.
//   - ErrLeadershipLost is [cluster.ErrLeadershipLost]: the node lost
//     leadership while the write was committing, which may still commit.
//   - ErrEnqueueTimeout and ErrRaftShutdown are [storage.ErrUnavailable]: raft
//     wrote nothing. That holds for a barrier, which writes nothing, and for a
//     write that raft never dispatched; the error of a write's own future goes
//     through writeError first, which gives a write that the leader dispatched,
//     and whose node shut down before its outcome was known, the meaning
//     [cluster.ErrLeadershipLost].
//
// Any other error, and nil, is returned as it is.
func clusterError(err error) error {
	var meaning error
	switch {
	case errors.Is(err, hraft.ErrNotLeader), errors.Is(err, hraft.ErrLeadershipTransferInProgress):
		meaning = cluster.ErrNotLeader
	case errors.Is(err, hraft.ErrLeadershipLost):
		meaning = cluster.ErrLeadershipLost
	case errors.Is(err, hraft.ErrEnqueueTimeout), errors.Is(err, hraft.ErrRaftShutdown):
		meaning = storage.ErrUnavailable
	default:
		return err
	}
	return meant{meaning: meaning, cause: err}
}

// meant is an error of hashicorp/raft with the meaning clusterError gave it. It
// is both for errors.Is and errors.As, and keeps the text of the cause.
type meant struct{ meaning, cause error }

func (e meant) Error() string   { return e.cause.Error() }
func (e meant) Unwrap() []error { return []error{e.meaning, e.cause} }

// configurationChanged begins the error that hashicorp/raft v1.8.0 gives a
// configuration change whose prevIndex is no longer the index of the latest
// configuration: "configuration changed since %v (latest is %v)", from
// nextConfiguration in its configuration.go. The error has no sentinel, so
// membershipError tells it by this text, and the mapping is tied to v1.8.0:
// TestStaleConfigurationIndexConflicts fails when an upgrade words it otherwise.
const configurationChanged = "configuration changed since "

// membershipError is writeError for a change of the configuration whose future
// is f: a change refused because the configuration changed since its prevIndex
// is [cluster.ErrConfigurationChanged], and still the error of hashicorp/raft.
func membershipError(f hraft.IndexFuture, err error) error {
	if err != nil && strings.HasPrefix(err.Error(), configurationChanged) {
		return meant{meaning: cluster.ErrConfigurationChanged, cause: err}
	}
	return writeError(f, err)
}

// barrierError is clusterError for the error of a barrier, such as the one that
// starts the first transaction of a leadership term (awaitReady). A barrier
// writes nothing, so one that fails with ErrLeadershipLost proposed no write:
// the transaction did not run, and the node is no longer the leader. That is
// ErrNotLeader, which a client may retry, not the ErrLeadershipLost of a write
// that may still commit.
func barrierError(err error) error {
	if errors.Is(err, hraft.ErrLeadershipLost) {
		return meant{meaning: cluster.ErrNotLeader, cause: err}
	}
	return clusterError(err)
}
