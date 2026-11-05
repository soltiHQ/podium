package raft

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/session"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// logged counts entries of a node's log: all of them, and the barriers and the
// commands among them, as reportCommits does for the benchmarks.
type logged struct{ entries, barriers, commands int }

// loggedAfter counts the entries of n's log after index after.
func loggedAfter(t *testing.T, n *loopbackNode, after uint64) logged {
	t.Helper()
	last, err := n.profile.logStore.LastIndex()
	if err != nil {
		t.Fatal(err)
	}
	var got logged
	for index := after + 1; index <= last; index++ {
		var entry hraft.Log
		if err := n.profile.logStore.GetLog(index, &entry); err != nil {
			t.Fatalf("%s: read log %d: %v", n.id, index, err)
		}
		got.entries++
		switch entry.Type {
		case hraft.LogBarrier:
			got.barriers++
		case hraft.LogCommand:
			got.commands++
		}
	}
	return got
}

// holdsAll waits until every node of c holds the agents ids.
func holdsAll(t *testing.T, c *loopbackCluster, ids ...string) {
	t.Helper()
	for _, n := range c.nodes {
		waitFor(t, 5*time.Second, fmt.Sprintf("%s holds %v", n.id, ids), func() bool {
			held := n.agents()
			for _, id := range ids {
				if !held[id] {
					return false
				}
			}
			return true
		})
	}
}

// refusedBeforeProposing fails the test unless err is the refusal of a write
// that proposed nothing: cluster.ErrNotLeader, which a client may retry, as
// the error of a barrier that lost the leadership is, and not
// cluster.ErrLeadershipLost, which says that the write may still commit.
func refusedBeforeProposing(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, cluster.ErrNotLeader) || errors.Is(err, cluster.ErrLeadershipLost) {
		t.Fatalf("%s = %v, want %v, and not %v", what, err, cluster.ErrNotLeader, cluster.ErrLeadershipLost)
	}
	if kind := errkind.Classify(err); kind != errkind.Unavailable {
		t.Fatalf("errkind.Classify(%v) = %v, want Unavailable", err, kind)
	}
}

// In a leadership term, the first transaction of the leader's store commits a
// barrier and every later one commits none: a write commits its command alone,
// and a transaction that writes nothing commits nothing at all. The heartbeat of
// an enrolled agent, as discovery runs it, is a token check that writes nothing
// while the token matches, a local read and an upsert: one command. Every write
// lands on every node. Three nodes on loopback with mutual TLS.
func TestOneBarrierPerTerm(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	leader := c.leader(t, 10*time.Second)
	term := leader.profile.raft.CurrentTerm()
	store := leader.profile.Store()
	svc := agent.New(store, zerolog.Nop())
	ctx := context.Background()

	// step runs what on the leader's store and returns what the leader's log
	// gained by it, in the term the test started in.
	step := func(what string, run func() error) logged {
		t.Helper()
		before := leader.profile.raft.LastIndex()
		if err := run(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if now := leader.profile.raft.CurrentTerm(); now != term {
			t.Fatalf("%s: the leadership term moved from %d to %d, which the counts cannot follow", what, term, now)
		}
		return loggedAfter(t, leader, before)
	}
	upsert := func(id string) func() error {
		return func() error { return store.UpsertAgent(ctx, testAgent(t, id)) }
	}

	if got, want := step("the first write", upsert("first")), (logged{entries: 2, barriers: 1, commands: 1}); got != want {
		t.Fatalf("the first transaction of the term logged %+v, want %+v: a barrier, then its command", got, want)
	}
	for i := range 3 {
		id := fmt.Sprintf("later-%d", i)
		if got, want := step("the write of "+id, upsert(id)), (logged{entries: 1, commands: 1}); got != want {
			t.Fatalf("the write of %s logged %+v, want %+v: its command alone", id, got, want)
		}
	}
	read := func() error {
		return store.WithTx(ctx, func(tx storage.Storage) error {
			_, err := tx.GetAgent(ctx, "first")
			return err
		})
	}
	if got := step("a transaction that only reads", read); got != (logged{}) {
		t.Fatalf("a transaction that writes nothing logged %+v, want nothing", got)
	}

	const token = "token-of-first"
	verify := func() error { return svc.VerifyOrEnrollToken(ctx, "first", token) }
	if got, want := step("the enrollment", verify), (logged{entries: 1, commands: 1}); got != want {
		t.Fatalf("the enrollment of a token logged %+v, want %+v", got, want)
	}
	for i := range 3 {
		heartbeat := func() error {
			if err := verify(); err != nil {
				return err
			}
			a, err := svc.Get(ctx, "first")
			if err != nil {
				return err
			}
			a.SetUptimeSeconds(int64(100 + i))
			_, err = svc.Upsert(ctx, a)
			return err
		}
		if got, want := step("a heartbeat", heartbeat), (logged{entries: 1, commands: 1}); got != want {
			t.Fatalf("heartbeat %d logged %+v, want %+v: the upsert's command alone", i, got, want)
		}
	}
	holdsAll(t, c, "first", "later-0", "later-1", "later-2")
}

// After a leadership change, the first transaction of the new leader takes a
// barrier before its function reads; the later transactions of its term take
// none. The new leader led an earlier term, whose barrier it passed: the
// readiness of that term does not carry into the new one. The writes of the
// previous leader are on every node at the end; that the barrier makes a first
// transaction read them while the FSM still replays the log is
// TestRaftFirstTransactionAfterElectionReadsRestoredState's, as moveLeadership
// waits for a barrier of its own on the new leader. Three nodes on loopback
// with mutual TLS.
func TestNewLeaderBarriersBeforeItsFirstTransaction(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	ctx := context.Background()
	first := c.leader(t, 10*time.Second)
	if err := first.profile.Store().UpsertAgent(ctx, testAgent(t, "first-term")); err != nil {
		t.Fatal(err)
	}
	written := []string{"first-term"}

	second := c.follower(t, first)
	moveLeadership(t, c, second.id)
	for i := range 20 {
		id := fmt.Sprintf("second-term-%02d", i)
		if err := second.profile.Store().UpsertAgent(ctx, testAgent(t, id)); err != nil {
			t.Fatalf("%s writes %s: %v", second.id, id, err)
		}
		written = append(written, id)
	}
	moveLeadership(t, c, first.id)

	store := first.profile.Store()
	before := first.profile.raft.LastIndex()
	var atRead logged // what the log gained when the function read
	err := store.WithTx(ctx, func(tx storage.Storage) error {
		atRead = loggedAfter(t, first, before)
		if _, err := tx.GetAgent(ctx, written[len(written)-1]); err != nil {
			return err
		}
		return tx.UpsertAgent(ctx, testAgent(t, "third-term"))
	})
	if err != nil {
		t.Fatalf("%s's first transaction of its new term: %v", first.id, err)
	}
	if want := (logged{entries: 1, barriers: 1}); atRead != want {
		t.Fatalf("%s's first transaction of its new term read once its log gained %+v, want %+v: a barrier", first.id, atRead, want)
	}
	if got, want := loggedAfter(t, first, before), (logged{entries: 2, barriers: 1, commands: 1}); got != want {
		t.Fatalf("%s's first transaction of its new term logged %+v, want %+v", first.id, got, want)
	}

	before = first.profile.raft.LastIndex()
	if err := store.UpsertAgent(ctx, testAgent(t, "third-term-later")); err != nil {
		t.Fatal(err)
	}
	if got, want := loggedAfter(t, first, before), (logged{entries: 1, commands: 1}); got != want {
		t.Fatalf("%s's second transaction of its new term logged %+v, want %+v: its command alone", first.id, got, want)
	}
	holdsAll(t, c, append(written, "third-term", "third-term-later")...)
}

// A leadership lost and regained while the barrier of a term's first
// transaction runs leaves the term the node leads at the barrier's end not
// ready: the barrier covers the term it ran in. The transaction fails with
// cluster.ErrNotLeader before its function runs, having proposed nothing, and
// the next transaction takes a barrier of its own. The seam is the store's
// barrier: after the real barrier, it moves the leadership to another node and
// back. Three nodes on loopback with mutual TLS.
func TestLeadershipChangeDuringBarrierLeavesTermNotReady(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	ctx := context.Background()
	leader := c.leader(t, 10*time.Second)
	other := c.follower(t, leader)
	store := leader.profile.store
	barrier := store.barrier
	var startTerm, endTerm uint64 // the terms at the start and the end of the first barrier
	moved := false
	store.barrier = func(ctx context.Context) error {
		if moved {
			return barrier(ctx)
		}
		moved = true
		startTerm = leader.profile.raft.CurrentTerm()
		if err := barrier(ctx); err != nil {
			return err
		}
		moveLeadership(t, c, other.id)
		moveLeadership(t, c, leader.id)
		endTerm = leader.profile.raft.CurrentTerm()
		return nil
	}

	before := leader.profile.raft.LastIndex()
	ran := false
	err := store.WithTx(ctx, func(tx storage.Storage) error {
		ran = true
		return tx.UpsertAgent(ctx, testAgent(t, "refused"))
	})
	if endTerm <= startTerm {
		t.Fatalf("the term went from %d to %d across the barrier, want a later one", startTerm, endTerm)
	}
	refusedBeforeProposing(t, "the transaction whose barrier saw the leadership move", err)
	if ran {
		t.Fatal("the transaction's function ran after a barrier that the leadership moved under")
	}
	if got := loggedAfter(t, leader, before); got.commands != 0 {
		t.Fatalf("the refused transaction logged %+v, want no command", got)
	}

	before = leader.profile.raft.LastIndex()
	if err := store.UpsertAgent(ctx, testAgent(t, "after")); err != nil {
		t.Fatal(err)
	}
	if got, want := loggedAfter(t, leader, before), (logged{entries: 2, barriers: 1, commands: 1}); got != want {
		t.Fatalf("the next transaction, in term %d, logged %+v, want %+v: a barrier of its own, then its command", endTerm, got, want)
	}
	holdsAll(t, c, "after")
	if leader.agents()["refused"] {
		t.Fatalf("%s holds the refused write", leader.id)
	}
}

// A term that changes after a transaction's function read the state, and
// before its write is proposed, refuses the write with cluster.ErrNotLeader:
// the write was computed from the state of the earlier term, and nothing was
// proposed. The node leads again when the write would be proposed, in a later
// term. The seam is the transaction's own function, which runs between the two:
// after its read, it moves the leadership to another node and back. Sent
// again, the write lands. Three nodes on loopback with mutual TLS.
func TestTermChangeBeforeApplyRefusesTheWrite(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	ctx := context.Background()
	leader := c.leader(t, 10*time.Second)
	other := c.follower(t, leader)
	store := leader.profile.Store()
	if err := store.UpsertAgent(ctx, testAgent(t, "read")); err != nil {
		t.Fatal(err)
	}

	before := leader.profile.raft.LastIndex()
	startTerm := leader.profile.raft.CurrentTerm()
	err := store.WithTx(ctx, func(tx storage.Storage) error {
		if _, err := tx.GetAgent(ctx, "read"); err != nil {
			return err
		}
		moveLeadership(t, c, other.id)
		moveLeadership(t, c, leader.id)
		return tx.UpsertAgent(ctx, testAgent(t, "refused"))
	})
	if now := leader.profile.raft.CurrentTerm(); now <= startTerm || !leader.profile.leadership.AmLeader() {
		t.Fatalf("%s is in term %d, leading: %v, want it to lead a term after %d", leader.id, now, leader.profile.leadership.AmLeader(), startTerm)
	}
	refusedBeforeProposing(t, "the transaction whose term changed before its write was proposed", err)
	if got := loggedAfter(t, leader, before); got.commands != 0 {
		t.Fatalf("the refused transaction logged %+v, want no command", got)
	}
	if leader.agents()["refused"] {
		t.Fatalf("%s holds the refused write", leader.id)
	}

	if err := store.UpsertAgent(ctx, testAgent(t, "refused")); err != nil {
		t.Fatalf("the refused write, sent again: %v", err)
	}
	holdsAll(t, c, "read", "refused")
}

// A transaction that skipped the barrier of its term and proposes nothing,
// because its function failed or wrote nothing, answers once a quorum confirmed
// that the node still leads (VerifyLeader). A deposed leader reads its own,
// stale state, where the session that a newer leader created is missing: the
// "not found" that a session revoke takes for done would answer success. So a
// check that fails answers in place of the function's verdict, with the meaning
// of a barrier's error: cluster.ErrNotLeader for a leadership lost or refused,
// storage.ErrUnavailable for a node that shut down. A check that passes leaves
// the verdict as it is, and logs nothing. The first transaction of a term,
// whose barrier committed through a quorum, and a transaction that proposes a
// write, which a deposed leader cannot commit, check nothing. The seam is the
// store's VerifyLeader. Three nodes on loopback with mutual TLS.
func TestWriteFreeTransactionConfirmsTheLeadership(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	leader := c.leader(t, 10*time.Second)
	store := leader.profile.store
	ctx := context.Background()
	var (
		checks   atomic.Int32 // the checks of the leadership the store made
		checkErr error        // what the next check answers, the real check's answer when nil
	)
	verify := store.verifyLeader
	store.verifyLeader = func() error {
		checks.Add(1)
		if checkErr != nil {
			return checkErr
		}
		return verify()
	}
	notFound := func(tx storage.Storage) error { // a failed function, as a revoke of a missing session
		_, err := tx.GetSession(ctx, "absent")
		return err
	}
	readOnly := func(tx storage.Storage) error { // a function that writes nothing
		_, err := tx.GetAgent(ctx, "ready")
		return err
	}

	if err := store.WithTx(ctx, notFound); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the first transaction of the term = %v, want %v", err, storage.ErrNotFound)
	}
	if err := store.UpsertAgent(ctx, testAgent(t, "ready")); err != nil {
		t.Fatal(err)
	}
	if n := checks.Load(); n != 0 {
		t.Fatalf("the first transaction of the term and a write checked the leadership %d times, want none", n)
	}

	for _, tc := range []struct {
		name   string
		answer error // what the check answers
		want   error // the meaning of the transaction's error
		not    error // a meaning it must not have
	}{
		{"leadership lost", hraft.ErrLeadershipLost, cluster.ErrNotLeader, cluster.ErrLeadershipLost},
		{"not the leader", hraft.ErrNotLeader, cluster.ErrNotLeader, cluster.ErrLeadershipLost},
		{"shut down", hraft.ErrRaftShutdown, storage.ErrUnavailable, cluster.ErrNotLeader},
	} {
		checkErr = tc.answer
		for _, fn := range []struct {
			name string
			run  func(storage.Storage) error
		}{{"a failed function", notFound}, {"a function that writes nothing", readOnly}} {
			before, checked := leader.profile.raft.LastIndex(), checks.Load()
			err := store.WithTx(ctx, fn.run)
			if !errors.Is(err, tc.want) || !errors.Is(err, tc.answer) || errors.Is(err, tc.not) || errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("%s, check answering %q: WithTx = %v, want %v and %v, not %v nor the function's verdict", fn.name, tc.name, err, tc.want, tc.answer, tc.not)
			}
			if kind := errkind.Classify(err); kind != errkind.Unavailable {
				t.Fatalf("errkind.Classify(%v) = %v, want Unavailable", err, kind)
			}
			if n := checks.Load() - checked; n != 1 {
				t.Fatalf("%s checked the leadership %d times, want once", fn.name, n)
			}
			if got := loggedAfter(t, leader, before); got != (logged{}) {
				t.Fatalf("%s logged %+v, want nothing", fn.name, got)
			}
		}
	}

	checkErr = nil
	before := leader.profile.raft.LastIndex()
	if err := store.WithTx(ctx, notFound); !errors.Is(err, storage.ErrNotFound) || errors.Is(err, cluster.ErrNotLeader) {
		t.Fatalf("a failed function, check passing: WithTx = %v, want the function's %v", err, storage.ErrNotFound)
	}
	if err := store.WithTx(ctx, readOnly); err != nil {
		t.Fatalf("a function that writes nothing, check passing: WithTx = %v, want nil", err)
	}
	if got := loggedAfter(t, leader, before); got != (logged{}) {
		t.Fatalf("two transactions that wrote nothing, their checks passing, logged %+v, want nothing", got)
	}

	// The session service answers the revoke of a missing session as done: on
	// a node whose leadership the check refuses, it answers the refusal. The
	// actor is checked only against a session found, so it is nil here.
	sessions := session.New(store, zerolog.Nop())
	checkErr = hraft.ErrLeadershipLost
	if err := sessions.Revoke(ctx, nil, session.RevokeRequest{ID: "absent"}); !errors.Is(err, cluster.ErrNotLeader) {
		t.Fatalf("a revoke of a missing session, the leadership refused = %v, want %v", err, cluster.ErrNotLeader)
	}
	checkErr = nil
	if err := sessions.Revoke(ctx, nil, session.RevokeRequest{ID: "absent"}); err != nil {
		t.Fatalf("a revoke of a missing session, the leadership confirmed = %v, want nil", err)
	}
}
