package raft

import (
	"context"
	"errors"
	"fmt"
	"strings"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

var _ cluster.Admin = (*Admin)(nil)

// Admin is the cluster administration of a raft node: the narrow
// [cluster.Admin] through which the cluster service reads the node's view of its
// cluster and changes the membership. It never hands out the hashicorp/raft
// node it works on.
type Admin struct {
	p *Profile
}

// Admin returns the cluster administration of p.
func (p *Profile) Admin() *Admin { return &Admin{p: p} }

// NodeID implements cluster.Admin.
func (a *Admin) NodeID() string { return a.p.cfg.NodeID }

// Role implements cluster.Admin with the node's hashicorp/raft state, in lower
// case: leader, follower, candidate or shutdown.
func (a *Admin) Role() string { return strings.ToLower(a.p.raft.State().String()) }

// Leader implements cluster.Admin.
func (a *Admin) Leader() (id, address string) {
	addr, sid := a.p.raft.LeaderWithID()
	return string(sid), string(addr)
}

// LastIndex implements cluster.Admin.
func (a *Admin) LastIndex() uint64 { return a.p.raft.LastIndex() }

// Servers implements cluster.Admin with the copy of the latest configuration
// that hashicorp/raft keeps apart from its main loop: it waits for nothing, and
// has no index (v1.8.0's GetConfiguration gives 0).
func (a *Admin) Servers() []cluster.Server {
	future := a.p.raft.GetConfiguration()
	if future.Error() != nil {
		return nil
	}
	return servers(future.Configuration())
}

// Configuration implements cluster.Admin from the node's own stores, as
// latestConfiguration reads them: hashicorp/raft v1.8.0 has no call that gives
// the latest configuration with its index. Its Term is the node's term, read
// before the stores.
func (a *Admin) Configuration() (cluster.Configuration, error) {
	term := a.p.raft.CurrentTerm()
	conf, index, err := latestConfiguration(a.p.logStore, a.p.snap)
	if err != nil {
		return cluster.Configuration{}, err
	}
	return cluster.Configuration{Term: term, Index: index, Servers: servers(conf)}, nil
}

// VerifyLeader implements cluster.Admin as a transaction of the store confirms
// its leadership (confirmLeadership): hashicorp/raft's VerifyLeader, which
// heartbeats the followers and writes nothing, within the bound of
// awaitLeadership, and then a node that still leads term. A check that fails
// is cluster.ErrNotLeader, or storage.ErrUnavailable once the node shut down.
func (a *Admin) VerifyLeader(ctx context.Context, term uint64) error {
	return confirmLeadership(ctx, a.p.raft, a.p.store.verifyLeader, term)
}

// Probe implements cluster.Admin with probe, within probeTimeout.
func (a *Admin) Probe(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := probe(ctx, address, a.p.security); err != nil {
		return fmt.Errorf("raft: probe %s: %w: %w", address, cluster.ErrPeerUnreachable, err)
	}
	return nil
}

// AddVoter implements cluster.Admin with hashicorp/raft's AddVoter, which
// returns once the change committed, or failed, or the node shut down first
// (awaitWrite). Its errors have the meaning membershipError gives them. A node
// that shut down already proposes nothing (refuseStopped).
func (a *Admin) AddVoter(id, address string, prevIndex uint64) (uint64, error) {
	if err := refuseStopped(a.p.store.stopped); err != nil {
		return 0, err
	}
	future := a.p.raft.AddVoter(hraft.ServerID(id), hraft.ServerAddress(address), prevIndex, 0)
	if err := awaitWrite(future, a.p.store.stopped); err != nil {
		return 0, membershipError(future, err)
	}
	return future.Index(), nil
}

// RemoveServer implements cluster.Admin as AddVoter does, with hashicorp/raft's
// RemoveServer.
func (a *Admin) RemoveServer(id string, prevIndex uint64) (uint64, error) {
	if err := refuseStopped(a.p.store.stopped); err != nil {
		return 0, err
	}
	future := a.p.raft.RemoveServer(hraft.ServerID(id), prevIndex, 0)
	if err := awaitWrite(future, a.p.store.stopped); err != nil {
		return 0, membershipError(future, err)
	}
	return future.Index(), nil
}

// TransferLeadership implements cluster.Admin with hashicorp/raft's
// LeadershipTransfer, which picks the voter with the highest next index and
// returns once this node left its leader loop, or after a failed transfer. That
// can pick a dead voter, if nothing was written since it died: the transfer
// then fails. A transfer that fails on its timeout may have sent its TimeoutNow
// already, so the target can still win and the node lose its leadership after
// the error. A node that does not lead, or hands over already, refuses with
// cluster.ErrNotLeader (clusterError). The wait needs no bound of its own: a
// goroutine of hashicorp/raft v1.8.0 answers the transfer on its timeout, an
// election timeout, or as the node leaves its leader loop, a shutdown included
// (leaderLoop).
func (a *Admin) TransferLeadership() error {
	return clusterError(a.p.raft.LeadershipTransfer().Error())
}

// servers returns the servers of conf in its order.
func servers(conf hraft.Configuration) []cluster.Server {
	out := make([]cluster.Server, 0, len(conf.Servers))
	for _, s := range conf.Servers {
		out = append(out, cluster.Server{ID: string(s.ID), Address: string(s.Address), Suffrage: suffrage(s.Suffrage)})
	}
	return out
}

// suffrage is the cluster.Suffrage of s.
func suffrage(s hraft.ServerSuffrage) cluster.Suffrage {
	switch s {
	case hraft.Voter:
		return cluster.Voter
	case hraft.Nonvoter:
		return cluster.Nonvoter
	default:
		return cluster.Staging
	}
}

// latestConfiguration reads the latest configuration of a node, and the index of
// the log entry that holds it, from its log and snapshot stores, as hashicorp/raft
// v1.8.0's NewRaft rebuilds it at start: the last LogConfiguration entry above the
// latest snapshot's index, or else the configuration the snapshot holds, whose
// index is at most its own. That is Raft's latest configuration, committed or not,
// and the one a configuration change's prevIndex is compared with (nextConfiguration).
//
// It reads while raft writes, and every race leaves the index it gives behind,
// never ahead: an entry appended after LastIndex was read, a snapshot taken or a
// compaction during the scan. A change against a stale index is refused, as
// cluster.ErrConfigurationChanged, and changes nothing. A gap in the log, which
// the LogStore contract permits after a snapshot restore, or an entry compacted
// meanwhile, is skipped. A store that gives no configuration at all, such as no
// readable snapshot meta once compaction dropped the configuration's entry,
// gives an empty one at index 0, which the cluster service refuses.
//
// The scan reads and decodes every entry above the latest snapshot: up to
// snapshot_threshold, plus what snapshot_interval times the write rate adds
// between two checks, under heavy writes. So it runs once per change, on the
// leader, and never for a status. While a snapshot is being written, List can
// log hashicorp/raft's WARN "found temporary snapshot".
func latestConfiguration(logs hraft.LogStore, snaps hraft.SnapshotStore) (hraft.Configuration, uint64, error) {
	var (
		floor uint64
		conf  hraft.Configuration
		index uint64
	)
	metas, err := snaps.List()
	if err != nil {
		return hraft.Configuration{}, 0, fmt.Errorf("raft: list snapshots: %w", err)
	}
	if len(metas) > 0 {
		floor, conf, index = metas[0].Index, metas[0].Configuration, metas[0].ConfigurationIndex
	}
	first, err := logs.FirstIndex()
	if err != nil {
		return hraft.Configuration{}, 0, fmt.Errorf("raft: first log index: %w", err)
	}
	last, err := logs.LastIndex()
	if err != nil {
		return hraft.Configuration{}, 0, fmt.Errorf("raft: last log index: %w", err)
	}
	for i := last; i > floor && i >= first && i > 0; i-- {
		var entry hraft.Log
		switch err := logs.GetLog(i, &entry); {
		case errors.Is(err, hraft.ErrLogNotFound):
			continue
		case err != nil:
			return hraft.Configuration{}, 0, fmt.Errorf("raft: read log %d: %w", i, err)
		}
		if entry.Type == hraft.LogConfiguration {
			return hraft.DecodeConfiguration(entry.Data), i, nil
		}
	}
	return conf, index, nil
}
