package raft

import (
	"fmt"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// bootstrapServers builds the initial Raft Configuration.Servers list: every
// peer discovery reports, as voters sorted by ID ([cluster.ServerList]).
//
// hashicorp/raft expects every node to bootstrap the same configuration, and
// once it has a configuration, a node rejects the vote of a candidate missing
// from it. So the list is used exactly as discovery reports it on every node,
// never rewritten for this one: a discovery error, an ID or address listed
// twice, or a list that does not name this node as cfg.NodeID at myAddr fails
// the start instead.
//
// This runs only on first start (HasExistingState=false). On subsequent
// restarts Raft loads its config from the BoltDB log and discovery is not
// asked.
func bootstrapServers(cfg Config, myAddr hraft.ServerAddress, disco cluster.Discovery) ([]hraft.Server, error) {
	var peers []cluster.Peer
	if disco != nil {
		var err error
		if peers, err = disco.Peers(contextBackground()); err != nil {
			return nil, fmt.Errorf("raft: discovery: %w", err)
		}
	}
	list, err := cluster.ServerList(peers, cfg.NodeID, string(myAddr))
	if err != nil {
		return nil, fmt.Errorf("raft: bootstrap servers: %w", err)
	}
	servers := make([]hraft.Server, 0, len(list))
	for _, p := range list {
		servers = append(servers, hraft.Server{Suffrage: hraft.Voter, ID: hraft.ServerID(p.ID), Address: hraft.ServerAddress(p.Address)})
	}
	return servers, nil
}
