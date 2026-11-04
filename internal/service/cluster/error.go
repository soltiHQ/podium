package cluster

import (
	"fmt"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// The errors of a membership change that the service refuses or that did not
// take place. Each changed nothing, and wraps the storage or cluster error that
// errkind classifies it by.
var (
	// ErrInvalidMember refuses a member without an ID or an address, an ID that
	// discovery.peers or the member's route could not name, or an address that
	// is not a host and a port from 1 to 65535.
	ErrInvalidMember = fmt.Errorf("a member needs an ID without /, =, a comma or surrounding spaces, and an address host:port with a port from 1 to 65535: %w", storage.ErrInvalidArgument)

	// ErrUnknownMember refuses the removal of an ID that no member of the
	// configuration has.
	ErrUnknownMember = fmt.Errorf("no member of the cluster has this ID: %w", storage.ErrNotFound)

	// ErrMemberExists refuses to add an ID that a member of the configuration
	// has already.
	ErrMemberExists = fmt.Errorf("a member of the cluster has this ID already: %w", storage.ErrAlreadyExists)

	// ErrAddressInUse refuses to add an address that another member of the
	// configuration uses, compared as written.
	ErrAddressInUse = fmt.Errorf("another member of the cluster uses this address: %w", storage.ErrAlreadyExists)

	// ErrForceRequired refuses, without force, a removal that leaves fewer than
	// minVoters voters.
	ErrForceRequired = fmt.Errorf("the removal leaves fewer than %d voters and needs force: %w", minVoters, storage.ErrConflict)

	// ErrLastVoter refuses the removal of the last voter, force or not:
	// hashicorp/raft has no configuration without a voter.
	ErrLastVoter = fmt.Errorf("the last voter cannot be removed: %w", storage.ErrConflict)

	// ErrTransferFailed reports a removal of the leader whose leadership
	// transfer failed: the node still leads and is still a member.
	ErrTransferFailed = fmt.Errorf("the leadership transfer failed, so nothing was removed: %w", storage.ErrUnavailable)

	// ErrLeadershipMoved reports a removal of the leader that handed its
	// leadership over: the node is still a member, and the removal must be
	// repeated at the new leader, where it removes a follower.
	ErrLeadershipMoved = fmt.Errorf("this node handed its leadership over and is still a member: repeat the removal at the new leader: %w", cluster.ErrNotLeader)
)
