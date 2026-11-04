package cluster

import "github.com/soltiHQ/control-plane/internal/cluster"

// Status is the cluster as one node sees it: its own ID and role, the leader it
// knows, by ID and raft address, both empty when it knows none, the servers of
// its latest configuration and the index of the last entry of its log. Its
// Fingerprint is the fingerprint of the settings every replica must share, as
// this node holds them (cluster.Fingerprint): nodes whose fingerprints differ
// do not share them.
type Status struct {
	NodeID      string
	Role        string
	LeaderID    string
	LeaderAddr  string
	Members     []cluster.Server
	LastIndex   uint64
	Fingerprint string
}

// AddRequest asks for a voter: its ID and the raft address, host:port, at which
// the other nodes reach it.
type AddRequest struct {
	ID      string
	Address string
}

// RemoveRequest asks for the removal of the member ID. Force allows a removal
// that leaves fewer than minVoters voters.
type RemoveRequest struct {
	ID    string
	Force bool
}
