package raft

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/rs/zerolog"
	bolt "go.etcd.io/bbolt"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// smoke reports whether every benchmark runs once, as `task ci/golang/bench`
// runs them: a check that they still work, not a measurement. The three-node
// benchmarks then take the smallest fleet only, to keep their setup to a few
// seconds.
func smoke() bool {
	f := flag.Lookup("test.benchtime")
	return f != nil && f.Value.String() == "1x"
}

// fleetSizes are the agent counts of the three-node benchmarks, those of the
// durable store's benchmarks, so that a result reads against its standalone twin.
func fleetSizes() []int {
	if smoke() {
		return []int{100}
	}
	return []int{100, 1000, 5000}
}

// settleTime is how long settle keeps a cluster committing: more than the 4.5 s
// that the slowest start seen took to settle.
const settleTime = 6 * time.Second

// settle commits barriers on the leader for settleTime, untimed, unless the
// benchmarks run once (smoke). A commit is four fdatasyncs in a row, two by the
// leader's log store and two by a follower's (bbolt syncs twice per
// transaction). On the machine of the README's numbers an fdatasync takes 0.4 ms
// while its disk is busy and 1 to 2 ms for the first seconds after it idled: a
// barrier then takes 8 ms where it settles at 2.2 ms. A timed loop that starts
// in that state measures the disk, not the commit, and its median is one of two
// levels from one run to the next. The slow state is no property of a new
// cluster (a settled one falls back into it after 8 s of idling) nor of raft (a
// bare fdatasync loop does the same).
func settle(b *testing.B, leader *loopbackNode) {
	b.Helper()
	if smoke() {
		return
	}
	ctx := context.Background()
	for end := time.Now().Add(settleTime); time.Now().Before(end); {
		if err := awaitApplied(ctx, leader.profile.raft); err != nil {
			b.Fatal(err)
		}
	}
}

// benchCluster starts three nodes on loopback with mutual TLS, as the
// multi-node tests do, and seeds the leader's store through raft with a fleet of
// agents: one command, which every node applies. It returns the leader once all
// three have applied the fleet and the cluster has settled, so that the timed
// loop starts on a cluster at rest. The nodes stop when the benchmark ends.
func benchCluster(b *testing.B, agents int) *loopbackNode {
	b.Helper()
	c := newLoopbackCluster(b, "n1", "n2", "n3")
	leader := c.leader(b, 10*time.Second)
	start := time.Now()
	if err := storagetest.Seed(context.Background(), leader.profile.Store(), storagetest.Fixture{Agents: agents}); err != nil {
		b.Fatal(err)
	}
	seeded := leader.profile.raft.LastIndex()
	waitFor(b, 30*time.Second, "every node applies the fleet", func() bool {
		for _, n := range c.nodes {
			if n.profile.raft.AppliedIndex() < seeded {
				return false
			}
		}
		return true
	})
	if testing.Verbose() {
		b.Logf("%d agents seeded through raft and applied on three nodes in %s", agents, time.Since(start).Round(time.Millisecond))
	}
	settle(b, leader)
	return leader
}

// reportCommits reports the entries that the leader's log gained since its last
// index was first, per iteration of b: all of them as commits/op, and the
// barriers and the commands among them. It reports the three after the timed
// loop, which has returned, and no split when the log no longer holds the
// entries, because a snapshot compacted them.
func reportCommits(b *testing.B, leader *loopbackNode, first uint64) {
	b.Helper()
	last := leader.profile.raft.LastIndex()
	b.ReportMetric(float64(last-first)/float64(b.N), "commits/op")
	var barriers, commands int
	for i := first + 1; i <= last; i++ {
		var entry hraft.Log
		if err := leader.profile.logStore.GetLog(i, &entry); err != nil {
			b.Logf("barriers/op and commands/op are not reported: %v", err)
			return
		}
		switch entry.Type {
		case hraft.LogBarrier:
			barriers++
		case hraft.LogCommand:
			commands++
		}
	}
	b.ReportMetric(float64(barriers)/float64(b.N), "barriers/op")
	b.ReportMetric(float64(commands)/float64(b.N), "commands/op")
}

// BenchmarkThreeNodeHeartbeat is BenchmarkHeartbeat of the durable store on the
// store of a leader of three nodes with mutual TLS on loopback: the heartbeat
// of an enrolled agent as discovery runs it. Verify the token, a transaction
// that writes nothing while the token matches, and so commits nothing; read
// the stored agent, which is local; then upsert the agent, one command. Only
// the first transaction of the leader's term commits a barrier, and the seed of
// benchCluster is that one, so a heartbeat commits one entry here, as the
// durable store does. The entry is replicated to a quorum and synced by the
// leader and a follower before it is acknowledged. The leader's store is the
// one measured. The followers apply every entry in the same process, so the
// allocations that the framework counts would be the three nodes', and the
// benchmark does not ask for them.
//
// It reports the entries that a heartbeat commits (commits/op) and how many of
// them are barriers and commands, as reportCommits does, and how many heartbeats
// tell the UI to refresh: none, as in the durable benchmark.
func BenchmarkThreeNodeHeartbeat(b *testing.B) {
	for _, n := range fleetSizes() {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			leader := benchCluster(b, n)
			svc := agent.New(leader.profile.Store(), zerolog.Nop())
			ctx := context.Background()
			first := leader.profile.raft.LastIndex()
			refreshes := 0
			storagetest.Measure(b, func(i int) error {
				// The payload is built per heartbeat, as discovery does. An agent
				// reports a longer uptime each time, or it would look restarted.
				p := storagetest.AgentParams(i % n)
				p.UptimeSeconds += int64(i)
				a, err := model.NewAgentFrom(p)
				if err != nil {
					return err
				}
				if err = svc.VerifyOrEnrollToken(ctx, p.ID, storagetest.AgentToken(i%n)); err != nil {
					return err
				}
				if _, err = svc.Get(ctx, p.ID); err != nil {
					return err
				}
				res, err := svc.Upsert(ctx, a)
				if res.Visible || res.Reconnected {
					refreshes++
				}
				return err
			})
			reportCommits(b, leader, first)
			b.ReportMetric(float64(refreshes)/float64(b.N), "refreshes/op")
		})
	}
}

// BenchmarkThreeNodeBarrier is the barrier that the first transaction of a
// leader's term starts with, on the cluster of BenchmarkThreeNodeHeartbeat:
// awaitApplied on the leader, one log entry replicated to a quorum and synced,
// which carries no write. A heartbeat in a term that its leader is ready for
// pays none; it paid two when every transaction took one, one for each of its
// two transactions (see "Performance" in the README).
func BenchmarkThreeNodeBarrier(b *testing.B) {
	for _, n := range fleetSizes() {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			leader := benchCluster(b, n)
			ctx := context.Background()
			first := leader.profile.raft.LastIndex()
			storagetest.Measure(b, func(int) error { return awaitApplied(ctx, leader.profile.raft) })
			reportCommits(b, leader, first)
		})
	}
}

// snapshotFloor is the index of the snapshot that aboveSnapshot makes.
const snapshotFloor = 100_000

// heartbeatCommand is the command that the upsert of a fixture agent proposes: a
// heartbeat's one command, and the size of the entries that aboveSnapshot fills
// the log with.
func heartbeatCommand(b testing.TB) []byte {
	b.Helper()
	a, err := model.NewAgentFrom(storagetest.AgentParams(0))
	if err != nil {
		b.Fatal(err)
	}
	tx := overlay.New(inmemory.New())
	if err = tx.UpsertAgent(context.Background(), a); err != nil {
		b.Fatal(err)
	}
	data, err := overlay.EncodeCommand(tx.Ops())
	if err != nil {
		b.Fatal(err)
	}
	return data
}

// aboveSnapshot builds the stores of a leader that has written since its last
// snapshot, as New opens them: a BoltDB log store and the snapshot store of
// New, in a data directory of their own, without a raft node, so that tens of
// thousands of entries cost a second of setup, not the minutes that raft would
// take to commit them. The latest snapshot is at snapshotFloor. Above it the
// log holds the entries of heavy writes, commands of the size of a heartbeat's,
// and the configuration entry of the three nodes at the bottom, the first index
// above the snapshot, which is the last that latestConfiguration reaches. It
// returns the stores and the index of the configuration entry.
func aboveSnapshot(b *testing.B, entries int) (hraft.LogStore, hraft.SnapshotStore, uint64) {
	b.Helper()
	dir := b.TempDir()
	snaps, err := newSnapshotStore(dir, zerolog.Nop())
	if err != nil {
		b.Fatal(err)
	}
	sink, err := snaps.Create(hraft.SnapshotVersionMax, snapshotFloor, 1, hraft.Configuration{}, 0, nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = sink.Write([]byte("state")); err != nil {
		b.Fatal(err)
	}
	if err = sink.Close(); err != nil {
		b.Fatal(err)
	}

	logs, err := boltdb.New(boltdb.Options{Path: filepath.Join(dir, "raft-log.bolt"), BoltOptions: &bolt.Options{Timeout: 250 * time.Millisecond}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = logs.Close() })

	conf := hraft.Configuration{Servers: []hraft.Server{
		{Suffrage: hraft.Voter, ID: "n1", Address: "10.0.0.1:7000"},
		{Suffrage: hraft.Voter, ID: "n2", Address: "10.0.0.2:7000"},
		{Suffrage: hraft.Voter, ID: "n3", Address: "10.0.0.3:7000"},
	}}
	const batch = 1000
	command := heartbeatCommand(b)
	if testing.Verbose() {
		b.Logf("%d entries of %d bytes above a snapshot at index %d, and a configuration at %d", entries, len(command), snapshotFloor, snapshotFloor+1)
	}
	pending := make([]*hraft.Log, 0, batch)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		if err := logs.StoreLogs(pending); err != nil {
			b.Fatal(err)
		}
		pending = pending[:0]
	}
	confIndex := uint64(snapshotFloor + 1)
	pending = append(pending, &hraft.Log{Index: confIndex, Term: 1, Type: hraft.LogConfiguration, Data: hraft.EncodeConfiguration(conf), AppendedAt: storagetest.Epoch})
	for i := range entries {
		pending = append(pending, &hraft.Log{Index: confIndex + 1 + uint64(i), Term: 1, Type: hraft.LogCommand, Data: command, AppendedAt: storagetest.Epoch})
		if len(pending) == batch {
			flush()
		}
	}
	flush()
	return logs, snaps, confIndex
}

// BenchmarkLatestConfiguration is latestConfiguration, which every change of the
// membership starts with, over a log with entries above the latest snapshot and
// the configuration entry below all of them, the worst place for the scan, which
// reads from the last entry down. 8192 entries are snapshot_threshold, about
// what a leader holds when its snapshot comes due under light writes; 70000 are
// the heavy-write worst case, snapshot_threshold plus snapshot_interval (120 s)
// at about 500 entries a second. Every entry is a command of the size of a
// heartbeat's, where a real log holds barriers too, which carry no data, so the
// log is the slowest to scan for its length.
func BenchmarkLatestConfiguration(b *testing.B) {
	for _, entries := range []int{8192, 70_000} {
		b.Run(strconv.Itoa(entries), func(b *testing.B) {
			logs, snaps, confIndex := aboveSnapshot(b, entries)
			b.ReportAllocs()
			storagetest.Measure(b, func(int) error {
				conf, index, err := latestConfiguration(logs, snaps)
				if err != nil {
					return err
				}
				if index != confIndex || len(conf.Servers) != 3 {
					return fmt.Errorf("latestConfiguration gave %d servers at index %d, want 3 at %d", len(conf.Servers), index, confIndex)
				}
				return nil
			})
		})
	}
}
