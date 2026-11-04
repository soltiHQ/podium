package raft

import (
	"errors"

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
//   - ErrLeadershipLost is [cluster.ErrLeadershipLost]: the node lost
//     leadership while the write was committing, which may still commit.
//   - ErrEnqueueTimeout and ErrRaftShutdown are [storage.ErrUnavailable]: raft
//     cannot take the write now.
//
// Any other error, and nil, is returned as it is.
func clusterError(err error) error {
	var meaning error
	switch {
	case errors.Is(err, hraft.ErrNotLeader):
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

// barrierError is clusterError for the error of the barrier that starts a
// transaction. A barrier writes nothing, so one that fails with ErrLeadershipLost
// proposed no write: the transaction did not run, and the node is no longer the
// leader. That is ErrNotLeader, which a client may retry, not the ErrLeadershipLost
// of a write that may still commit.
func barrierError(err error) error {
	if errors.Is(err, hraft.ErrLeadershipLost) {
		return meant{meaning: cluster.ErrNotLeader, cause: err}
	}
	return clusterError(err)
}
