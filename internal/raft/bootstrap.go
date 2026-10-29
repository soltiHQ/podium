package raft

import (
	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// bootstrapServers builds the initial Raft Configuration.Servers list.
//
// Rules:
//
//  1. Every peer returned by discovery is included. Discovery can return IPs
//     (dns driver) or FQDNs/static addresses (static driver).
//
//  2. The peer whose address matches our own advertise address is
//     recognised as self — we override its ID with cfg.NodeID so the
//     Raft node's local identity matches what other replicas see in the
//     bootstrap config.
//
//  3. If no peer matches self, self is appended explicitly — single-node
//     case or misconfigured discovery.
//
//  4. The list is deduped by address: a duplicate address is a fatal error
//     in Raft.BootstrapCluster, and discovery drivers may return self in
//     addition to explicit self-insertion.
//
// This runs only on first start (HasExistingState=false). On subsequent
// restarts Raft loads its config from the BoltDB log.
func bootstrapServers(cfg Config, myAddr hraft.ServerAddress, disco cluster.Discovery) ([]hraft.Server, error) {
	var (
		selfID   = hraft.ServerID(cfg.NodeID)
		seenAddr = make(map[hraft.ServerAddress]bool)
		seenID   = make(map[hraft.ServerID]bool)
		out      []hraft.Server
	)

	add := func(id hraft.ServerID, addr hraft.ServerAddress) {
		if seenAddr[addr] || seenID[id] {
			return
		}
		seenAddr[addr] = true
		seenID[id] = true
		out = append(out, hraft.Server{ID: id, Address: addr})
	}

	if disco != nil {
		peers, _ := disco.Peers(contextBackground())
		for _, p := range peers {
			addr := hraft.ServerAddress(p.Address)
			id := hraft.ServerID(p.ID)
			// If this peer is us (its address OR id matches ours), force
			// its ID to selfID so the local identity matches the config
			// and we don't get a duplicate ID from a separate "self"
			// entry below. Address match handles dns-discovery (peer IDs
			// are IPs); ID match handles static-discovery where user
			// listed self by its NodeID.
			if addr == myAddr || id == selfID {
				id = selfID
				addr = myAddr
			}
			add(id, addr)
		}
	}
	// Ensure self is in the list even if discovery missed it.
	add(selfID, myAddr)

	return out, nil
}
