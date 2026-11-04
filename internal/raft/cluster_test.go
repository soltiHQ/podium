package raft

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// testAgent returns an agent whose ID and name are id.
func testAgent(t *testing.T, id string) *model.Agent {
	t.Helper()
	agent, err := model.NewAgent(id, id, "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// errNoAnswer is the outcome of a write that did not return within its bound.
var errNoAnswer = errors.New("the write did not return")

// answerWithin runs write and returns its error, or errNoAnswer when it has not
// returned after timeout, and leaves it running. A write on a leader that shuts
// down as the write commits can wait for ever: hashicorp/raft v1.8.0 hands a
// committed write to its FSM goroutine, which returns at shutdown without
// draining what it was handed (runFSM), and the future of a write waits for no
// shutdown (ApplyLog sets none).
func answerWithin(timeout time.Duration, write func() error) error {
	answer := make(chan error, 1)
	go func() { answer <- write() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-answer:
		return err
	case <-timer.C:
		return errNoAnswer
	}
}

// 200 writes go one after the other to whichever node leads, and the leader is
// killed during the 100th: once the write's transaction has checked it, and as
// the leader proposes it, the leader's raft shuts down, and with it its
// transport. A write that returned nil is acknowledged. A write of unknown
// outcome, one that failed with cluster.ErrLeadershipLost after it was
// proposed, or that never returned, may land or not, and is not sent again. A
// write that failed with cluster.ErrNotLeader, storage.ErrUnavailable or the
// timeout of its barrier goes to the next leader: the upsert of an agent is
// the same write the second time. Once the two survivors hold the same state,
// every acknowledged write is in it, and a write of unknown outcome is on both
// survivors or on neither.
func TestFailoverKeepsAcknowledgedWrites(t *testing.T) {
	const writes, killAt = 200, 100
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	ctx := context.Background()
	var (
		acknowledged, unknown []string
		killed                *loopbackNode         // the leader that write killAt killed
		down                  = make(chan struct{}) // closed once killed is down
	)
	leader := c.leader(t, 10*time.Second)
	for i := 1; i <= writes; i++ {
		agent := testAgent(t, fmt.Sprintf("write-%03d", i))
		for attempt := 1; ; attempt++ {
			if attempt > 10 {
				t.Fatalf("%s was refused %d times before it was proposed", agent.ID(), attempt-1)
			}
			store := leader.profile.Store()
			write := func() error { return store.UpsertAgent(ctx, agent) }
			var fired chan struct{} // closed once the write started the kill
			if i == killAt && killed == nil {
				fired = make(chan struct{})
				doomed := leader.profile
				write = func() error {
					return store.WithTx(ctx, func(tx storage.Storage) error {
						if err := tx.UpsertAgent(ctx, agent); err != nil {
							return err
						}
						close(fired)
						go func() {
							defer close(down)
							_ = doomed.Shutdown()
						}()
						return nil
					})
				}
			}
			err := answerWithin(2*applyTimeout, write)
			if fired != nil {
				select {
				case <-fired:
					killed = leader
					select {
					case <-down:
					case <-time.After(10 * time.Second):
						t.Fatalf("%s did not shut down within 10s", killed.id)
					}
					killed.stop()
				default:
					if errors.Is(err, errNoAnswer) {
						t.Fatalf("%s did not return, and never reached the kill", agent.ID())
					}
				}
			}
			retry := false
			switch {
			case err == nil:
				acknowledged = append(acknowledged, agent.ID())
			case errors.Is(err, cluster.ErrLeadershipLost), errors.Is(err, errNoAnswer):
				unknown = append(unknown, agent.ID())
			case errors.Is(err, cluster.ErrNotLeader), errors.Is(err, storage.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
				retry = true
			default:
				t.Fatalf("%s: %v", agent.ID(), err)
			}
			if !retry {
				break
			}
			leader = c.leader(t, 10*time.Second)
		}
		if leader.profile == nil {
			leader = c.leader(t, 10*time.Second)
		}
	}
	if killed == nil {
		t.Fatalf("write %d killed no leader", killAt)
	}
	if got := len(acknowledged) + len(unknown); got != writes {
		t.Fatalf("%d writes acknowledged and %d of unknown outcome, want %d in all", len(acknowledged), len(unknown), writes)
	}

	var survivors []*loopbackNode
	for _, n := range c.nodes {
		if n != killed {
			survivors = append(survivors, n)
		}
	}
	// leader took the last write, and so holds every write it acknowledged.
	for _, n := range survivors {
		waitFor(t, 10*time.Second, n.id+" holds the state of "+leader.id, func() bool {
			return bytes.Equal(n.state(t), leader.state(t))
		})
	}
	for _, n := range survivors {
		held := n.agents()
		for _, id := range acknowledged {
			if !held[id] {
				t.Fatalf("%s lacks %s, an acknowledged write, after the failover from %s", n.id, id, killed.id)
			}
		}
	}
	held, landed := leader.agents(), 0
	for _, id := range unknown {
		if held[id] {
			landed++
		}
	}
	t.Logf("%s killed at write %d: %d writes acknowledged, %d of unknown outcome, %d of which landed", killed.id, killAt, len(acknowledged), len(unknown), landed)
}

// snapshotsAbove returns, newest first, the snapshots n holds above index
// floor.
func snapshotsAbove(t *testing.T, n *loopbackNode, floor uint64) []*hraft.SnapshotMeta {
	t.Helper()
	metas, err := n.profile.snap.List()
	if err != nil {
		t.Fatalf("%s: list snapshots: %v", n.id, err)
	}
	var above []*hraft.SnapshotMeta
	for _, meta := range metas {
		if meta.Index > floor {
			above = append(above, meta)
		}
	}
	return above
}

// snapshotAgents returns the IDs of the agents that n's snapshot id holds.
func snapshotAgents(t *testing.T, n *loopbackNode, id string) map[string]bool {
	t.Helper()
	_, state, err := n.profile.snap.Open(id)
	if err != nil {
		t.Fatalf("%s: open snapshot %s: %v", n.id, id, err)
	}
	defer func() { _ = state.Close() }()
	data, err := io.ReadAll(state)
	if err != nil {
		t.Fatal(err)
	}
	content, err := snapshot.Unmarshal(data)
	if err != nil {
		t.Fatalf("%s: snapshot %s: %v", n.id, id, err)
	}
	ids := make(map[string]bool, len(content.Agents))
	for _, agent := range content.Agents {
		ids[agent.ID()] = true
	}
	return ids
}

// checkPerm fails the test unless path has exactly the permission bits want.
func checkPerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s has mode %v, want %v", path, got, want)
	}
}

// Under a stream of writes, with small compaction settings, the leader compacts
// its log past the entries a stopped follower lacks, so that the follower,
// started again, can catch up only through a snapshot: it installs the
// leader's, as the writes go on, then applies the log entries that follow it,
// and holds the state of the other two. The snapshot it installed is private,
// as one it takes: its directory 0700, its files 0600. Started once more, it
// holds the state of that snapshot, writes made while it was down included, as
// soon as it starts, before a leader tells it that any entry is committed: New
// restored it from the snapshot. Once it has applied the entries after the
// snapshot, it holds the others' state again. The follower restarts with the
// default compaction settings, under which it takes no snapshot of its own
// during the test: one of its own would soon replace the leader's.
func TestSnapshotAndRestoreUnderLoad(t *testing.T) {
	const threshold, trailing = 16, 8
	compact := func(c *Config) {
		c.SnapshotInterval, c.SnapshotThreshold, c.TrailingLogs = 25*time.Millisecond, threshold, trailing
	}
	c := loopbackNodes(t, "n1", "n2", "n3")
	for _, n := range c.nodes {
		n.start(t, c.discovery(t), compact)
	}
	ctx := context.Background()
	writes := 0
	write := func(stage string) {
		t.Helper()
		writes++
		agent := testAgent(t, fmt.Sprintf("%s-%03d", stage, writes))
		c.write(t, 10*time.Second, func(s storage.Storage) error { return s.UpsertAgent(ctx, agent) })
	}
	for range 10 {
		write("before")
	}
	lagging := c.follower(t, c.leader(t, 10*time.Second))
	behind := lagging.profile.raft.LastIndex()
	lagging.stop()

	// The writes go on until the leader's log no longer holds the entry after
	// the last one the stopped follower holds.
	compacted := func() bool {
		first, err := c.leader(t, 10*time.Second).profile.logStore.FirstIndex()
		return err == nil && first > behind+1
	}
	deadline := time.Now().Add(20 * time.Second)
	for down := 0; down < 40 || !compacted(); down++ {
		if time.Now().After(deadline) {
			t.Fatalf("the leader's log still holds index %d after %d writes", behind+1, down)
		}
		write("down")
	}

	lagging.start(t, c.discovery(t))
	deadline = time.Now().Add(20 * time.Second)
	for after := 0; after < 20 || len(snapshotsAbove(t, lagging, behind)) == 0; after++ {
		if time.Now().After(deadline) {
			t.Fatalf("%s installed no snapshot within 20s, %d writes after its start", lagging.id, after)
		}
		write("catch-up")
	}
	leader := c.leader(t, 10*time.Second)
	for _, n := range c.nodes {
		waitFor(t, 10*time.Second, n.id+" holds the state of "+leader.id, func() bool {
			return bytes.Equal(n.state(t), leader.state(t))
		})
	}

	installed := snapshotsAbove(t, lagging, behind)
	for _, meta := range installed {
		dir := filepath.Join(lagging.dir, snapshotsDir, meta.ID)
		checkPerm(t, dir, 0o700)
		for _, name := range []string{"meta.json", "state.bin"} {
			checkPerm(t, filepath.Join(dir, name), 0o600)
		}
	}
	if open := openEntries(t, lagging.dir); len(open) > 0 {
		t.Fatalf("open to the group or others after %s installed a snapshot: %v", lagging.id, open)
	}

	// New restores the latest snapshot before it returns, and a follower applies
	// no entry of its log before a leader tells it the entry is committed.
	latest := installed[0]
	inSnapshot := snapshotAgents(t, lagging, latest.ID)
	missed := false // the snapshot holds a write made while lagging was down
	for id := range inSnapshot {
		missed = missed || strings.HasPrefix(id, "down-")
	}
	if !missed {
		t.Fatalf("the snapshot %s holds no write made while %s was down", latest.ID, lagging.id)
	}
	lagging.stop()
	lagging.start(t, c.discovery(t))
	restored := lagging.agents()
	for id := range inSnapshot {
		if !restored[id] {
			t.Fatalf("%s started without %s, which its snapshot %s holds", lagging.id, id, latest.ID)
		}
	}
	waitFor(t, 10*time.Second, lagging.id+" holds the state of "+leader.id+" again", func() bool {
		return bytes.Equal(lagging.state(t), leader.state(t))
	})
}

// A node that stops and starts again from its data directory rejoins its
// cluster. Here it is the leader, so it comes back as a follower of a later
// term. It restarts from its own raft state, its log as long as when it
// stopped, not from a new bootstrap; it follows the node that leads now, with
// the configuration of the three; it catches up the writes made while it was
// down from the leader's log, with no snapshot, and holds the state of the
// others; and it votes again: with another node stopped, it and the third
// elect a leader and commit a write.
func TestRejoinAfterRestart(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	ctx := context.Background()
	write := func(id string) {
		t.Helper()
		agent := testAgent(t, id)
		c.write(t, 10*time.Second, func(s storage.Storage) error { return s.UpsertAgent(ctx, agent) })
	}
	for i := range 5 {
		write(fmt.Sprintf("before-%d", i))
	}
	restarted := c.leader(t, 10*time.Second)
	members := configuration(t, restarted)
	stoppedAt := restarted.profile.raft.LastIndex()
	restarted.stop()
	for i := range 5 {
		write(fmt.Sprintf("while-down-%d", i))
	}

	restarted.start(t, c.discovery(t))
	if last := restarted.profile.raft.LastIndex(); last < stoppedAt {
		t.Fatalf("%s restarted with a log up to %d, want %d at least, where it stopped: it did not restart from its own state", restarted.id, last, stoppedAt)
	}
	waitFor(t, 10*time.Second, restarted.id+" follows another node", func() bool {
		id, _ := restarted.profile.Admin().Leader()
		return id != "" && id != restarted.id
	})
	if got := configuration(t, restarted); !slices.Equal(got, members) {
		t.Fatalf("%s rejoined with the configuration %v, want %v", restarted.id, got, members)
	}
	leader := c.leader(t, 10*time.Second)
	waitFor(t, 10*time.Second, restarted.id+" holds the state of "+leader.id, func() bool {
		return bytes.Equal(restarted.state(t), leader.state(t))
	})
	if snaps := snapshotsAbove(t, restarted, 0); len(snaps) != 0 {
		t.Fatalf("%s holds %d snapshots, want none: it caught up from the leader's log", restarted.id, len(snaps))
	}
	for i := range 5 {
		if id := fmt.Sprintf("while-down-%d", i); !restarted.agents()[id] {
			t.Fatalf("%s lacks %s", restarted.id, id)
		}
	}

	other := leader
	if other == restarted {
		other = c.nodes[slices.IndexFunc(c.nodes, func(n *loopbackNode) bool { return n != restarted })]
	}
	other.stop()
	write("after-the-rejoin")
	waitFor(t, 10*time.Second, restarted.id+" holds the write made with "+other.id+" down", func() bool {
		return restarted.agents()["after-the-rejoin"]
	})
}
