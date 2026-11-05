// Package cluster implements the membership use-cases of a raft cluster, on
// the leader, through the node's [cluster.Admin]:
//   - Status: this node's view of its cluster
//   - AddMember: a voter added at an address where a node of the cluster
//     answers, under one configuration index
//   - RemoveMember: a member removed under the force guard, the leader only
//     once it handed its leadership over.
//
// It holds the rules; the handler maps their errors to HTTP.
package cluster
