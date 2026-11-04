package cluster

import (
	"errors"
	"fmt"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// The errors of a membership change that the service refuses or that did not
// take place. Each changed nothing, and wraps the storage or cluster error that
// errkind classifies it by, but ErrConfigurationUnreadable, which wraps none.
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

	// ErrAddressInUse refuses to add an address at which a member of the
	// configuration answers already: its address as written, or another name of
	// an ip:port where it answers (AddressInUseError).
	ErrAddressInUse = fmt.Errorf("another member of the cluster uses this address: %w", storage.ErrAlreadyExists)

	// ErrConfigurationUnreadable refuses every change while the leader's stores
	// give no configuration to check it against: an error, or a configuration at
	// index 0 or with no server, which a leader never has (its configuration is
	// at index 1 at least). Index 0 would also turn off hashicorp/raft's check of
	// prevIndex. errkind classifies it as Internal.
	ErrConfigurationUnreadable = errors.New("cannot read the raft configuration")

	// ErrForceRequired refuses, without force, a removal that leaves fewer than
	// minVoters voters.
	ErrForceRequired = fmt.Errorf("the removal leaves fewer than %d voters and needs force: %w", minVoters, storage.ErrConflict)

	// ErrLastVoter refuses the removal of the last voter, force or not:
	// hashicorp/raft has no configuration without a voter.
	ErrLastVoter = fmt.Errorf("the last voter cannot be removed: %w", storage.ErrConflict)

	// ErrTransferFailed reports a removal of the leader whose leadership
	// transfer failed: nothing was removed, and the node is still a member.
	// Whether it still leads is not known: after a transfer that timed out, its
	// target can still win the election it was told to start.
	ErrTransferFailed = fmt.Errorf("the leadership transfer failed, so nothing was removed: %w", storage.ErrUnavailable)

	// ErrLeadershipMoved reports a removal of the leader that handed its
	// leadership over: the node is still a member, and the removal must be
	// repeated at the new leader, where it removes a follower.
	ErrLeadershipMoved = fmt.Errorf("this node handed its leadership over and is still a member: repeat the removal at the new leader: %w", cluster.ErrNotLeader)
)

// AddressInUseError refuses to add an address at which a member answers
// already: Member is the ID of that member, and At where both answer, the
// address as written or an ip:port that both resolve to. Two names of one
// ip:port would make two voters of one node, which raise the quorum and both
// leave with that node. It is ErrAddressInUse.
type AddressInUseError struct {
	Member string
	At     string
}

func (e *AddressInUseError) Error() string {
	return fmt.Sprintf("member %s answers at %s already: %v", e.Member, e.At, ErrAddressInUse)
}

func (e *AddressInUseError) Unwrap() error { return ErrAddressInUse }
