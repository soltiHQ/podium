package cluster

import (
	"context"
	"errors"
)

// ErrConfigurationChanged indicates a membership change refused because the
// configuration of the cluster changed since it was read: the change was checked
// against a configuration that is no longer the latest, and changed nothing. It
// can be read again and retried. The transports answer it as a conflict.
var ErrConfigurationChanged = errors.New("the cluster configuration changed since it was read")

// ErrPeerUnreachable indicates an address at which no node of this cluster
// answers as its raft transport would dial it: nothing listens there, or what
// listens presents a certificate the nodes refuse. A membership change that it
// stops changed nothing.
var ErrPeerUnreachable = errors.New("no node of this cluster answers at the address")

// The roles of a raft node, as [Admin.Role] names them: the states of
// hashicorp/raft.
const (
	RoleLeader    = "leader"
	RoleFollower  = "follower"
	RoleCandidate = "candidate"
	RoleShutdown  = "shutdown"
)

// Suffrage says whether a server of a raft configuration votes.
type Suffrage string

// The suffrages of hashicorp/raft. The membership API adds voters only.
const (
	Voter    Suffrage = "voter"
	Nonvoter Suffrage = "nonvoter"
	Staging  Suffrage = "staging"
)

// Server is a server of a raft configuration: its ID, the address the others
// reach it at, as written, and its vote.
type Server struct {
	ID       string
	Address  string
	Suffrage Suffrage
}

// Configuration is a raft configuration and the index of the log entry that
// holds it, which a change passes on to be applied to this configuration only.
type Configuration struct {
	Index   uint64
	Servers []Server
}

// Voters counts the servers of c that vote.
func (c Configuration) Voters() int {
	n := 0
	for _, s := range c.Servers {
		if s.Suffrage == Voter {
			n++
		}
	}
	return n
}

// Admin is implemented by drivers whose cluster membership can change: raft. It
// is the narrow interface through which the cluster service reads this node's
// view of its cluster and changes the membership. Standalone has none.
type Admin interface {
	// NodeID returns this node's ID.
	NodeID() string
	// Role returns this node's role: RoleLeader, RoleFollower, RoleCandidate or
	// RoleShutdown.
	Role() string
	// Leader returns the ID and the raft address of the leader this node knows,
	// both empty when it knows none.
	Leader() (id, address string)
	// LastIndex returns the index of the last entry of this node's log, or of
	// its latest snapshot when that is later.
	LastIndex() uint64
	// Servers returns the servers of this node's latest configuration, read
	// cheaply and without its index.
	Servers() []Server
	// Configuration returns this node's latest configuration with its index,
	// which a change is applied against.
	Configuration() (Configuration, error)
	// Probe dials address as the raft transport dials a peer and reports, with
	// ErrPeerUnreachable, when no node of this cluster answers there.
	Probe(ctx context.Context, address string) error
	// AddVoter adds a voter id at address to the configuration whose index is
	// prevIndex, and returns the index of the new configuration. A
	// configuration that is no longer the latest fails it with
	// ErrConfigurationChanged.
	AddVoter(id, address string, prevIndex uint64) (uint64, error)
	// RemoveServer removes the server id from the configuration whose index is
	// prevIndex, as AddVoter changes it.
	RemoveServer(id string, prevIndex uint64) (uint64, error)
	// TransferLeadership hands this node's leadership over to another voter and
	// returns once this node no longer leads, or with the error of a transfer
	// that failed, after which it still leads.
	TransferLeadership() error
}
