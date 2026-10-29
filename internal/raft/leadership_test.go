package raft

import (
	"context"
	"io"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type leadershipTestNode struct {
	id         hraft.ServerID
	r          *hraft.Raft
	transport  *hraft.InmemTransport
	leadership *Leadership
}

func newLeadershipTestCluster(t *testing.T) []*leadershipTestNode {
	t.Helper()
	nodes := make([]*leadershipTestNode, 3)
	for i := range nodes {
		addr, transport := hraft.NewInmemTransportWithTimeout("", 50*time.Millisecond)
		nodes[i] = &leadershipTestNode{id: hraft.ServerID(addr), transport: transport}
	}
	for i, a := range nodes {
		for j, b := range nodes {
			if i != j {
				a.transport.Connect(b.transport.LocalAddr(), b.transport)
			}
		}
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			if n.r != nil {
				_ = n.r.Shutdown().Error()
			}
			_ = n.transport.Close()
		}
	})
	configuration := hraft.Configuration{}
	for _, n := range nodes {
		cfg := hraft.DefaultConfig()
		cfg.LocalID = n.id
		cfg.HeartbeatTimeout = 100 * time.Millisecond
		cfg.ElectionTimeout = 150 * time.Millisecond
		cfg.LeaderLeaseTimeout = 75 * time.Millisecond
		cfg.CommitTimeout = 10 * time.Millisecond
		cfg.LogOutput = io.Discard
		logs := hraft.NewInmemStore()
		var err error
		n.r, err = hraft.NewRaft(cfg, NewFSM(inmemory.New(), event.NewHub(zerolog.Nop())), logs, logs, hraft.NewInmemSnapshotStore(), n.transport)
		if err != nil {
			t.Fatal(err)
		}
		n.leadership = NewLeadership(n.r)
		configuration.Servers = append(configuration.Servers, hraft.Server{ID: n.id, Address: n.transport.LocalAddr(), Suffrage: hraft.Voter})
	}
	for _, n := range nodes {
		if err := n.r.BootstrapCluster(configuration).Error(); err != nil {
			t.Fatal(err)
		}
	}
	return nodes
}

func leadershipTestLeader(t *testing.T, nodes []*leadershipTestNode) *leadershipTestNode {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var leader *leadershipTestNode
		count := 0
		for _, n := range nodes {
			if n.leadership.AmLeader() {
				leader = n
				count++
			}
		}
		if count == 1 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := leader.leadership.WaitReady(ctx)
			cancel()
			if err == nil {
				return leader
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cluster did not elect a ready unique leader")
	return nil
}

func leadershipTestReceive(t *testing.T, ch <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-ch:
		return ctx
	case <-time.After(3 * time.Second):
		t.Fatal("leader callback did not run")
		return nil
	}
}

func leadershipTestCanceled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("leader context was not canceled")
	}
}

func TestLeadershipEveryConsumerCancelsOnLossAndRunsAfterRegain(t *testing.T) {
	nodes := newLeadershipTestCluster(t)
	leader := leadershipTestLeader(t, nodes)
	outer, cancel := context.WithCancel(context.Background())
	defer cancel()
	callbacks := make([]chan context.Context, 3)
	done := make([]chan error, 3)
	first := make([]context.Context, 3)
	for i := range callbacks {
		callbacks[i] = make(chan context.Context, 2)
		done[i] = make(chan error, 1)
		go func(i int) {
			done[i] <- leader.leadership.WhenLeader(outer, func(ctx context.Context) error {
				select {
				case callbacks[i] <- ctx:
				case <-ctx.Done():
					return nil
				}
				<-ctx.Done()
				return nil
			})
		}(i)
	}
	for i := range first {
		first[i] = leadershipTestReceive(t, callbacks[i])
		if first[i].Err() != nil {
			t.Fatal("new leader context already canceled")
		}
	}
	majority := make([]*leadershipTestNode, 0, 2)
	for _, n := range nodes {
		if n != leader {
			leader.transport.Disconnect(n.transport.LocalAddr())
			n.transport.Disconnect(leader.transport.LocalAddr())
			majority = append(majority, n)
		}
	}
	for _, ctx := range first {
		leadershipTestCanceled(t, ctx)
	}
	next := leadershipTestLeader(t, majority)
	for _, n := range majority {
		leader.transport.Connect(n.transport.LocalAddr(), n.transport)
		n.transport.Connect(leader.transport.LocalAddr(), leader.transport)
	}
	// An applied leader barrier is only meaningful after the healed follower
	// catches up; replication/transfer future supplies that bounded handoff.
	transferred := make(chan error, 1)
	go func() {
		transferred <- next.r.LeadershipTransferToServer(leader.id, leader.transport.LocalAddr()).Error()
	}()
	select {
	case err := <-transferred:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leadership transfer did not complete")
	}
	if got := leadershipTestLeader(t, nodes); got != leader {
		t.Fatal("leadership did not return to original node")
	}
	second := make([]context.Context, 3)
	for i := range second {
		second[i] = leadershipTestReceive(t, callbacks[i])
		if second[i] == first[i] || second[i].Err() != nil {
			t.Fatal("regained leadership reused a canceled lease")
		}
	}
	cancel()
	for _, ctx := range second {
		leadershipTestCanceled(t, ctx)
	}
	for _, ch := range done {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("WhenLeader did not drain on parent cancellation")
		}
	}
}

func TestLeadershipShutdownCancelsEveryConsumerWithoutParentCancel(t *testing.T) {
	nodes := newLeadershipTestCluster(t)
	leader := leadershipTestLeader(t, nodes)
	outer, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make([]chan error, 3)
	contexts := make([]context.Context, 3)
	for i := range done {
		entered := make(chan context.Context, 1)
		done[i] = make(chan error, 1)
		go func(i int) {
			done[i] <- leader.leadership.WhenLeader(outer, func(ctx context.Context) error { entered <- ctx; <-ctx.Done(); return nil })
		}(i)
		contexts[i] = leadershipTestReceive(t, entered)
	}
	if err := leader.r.Shutdown().Error(); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range contexts {
		leadershipTestCanceled(t, ctx)
	}
	for _, ch := range done {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Raft shutdown did not release observer owner")
		}
	}
	if outer.Err() != nil {
		t.Fatal("test relied on outer cancellation")
	}
}

func TestLeadershipFollowerParentCancelAndCallbackError(t *testing.T) {
	nodes := newLeadershipTestCluster(t)
	leader := leadershipTestLeader(t, nodes)
	var follower *leadershipTestNode
	for _, n := range nodes {
		if n != leader {
			follower = n
			break
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- follower.leadership.WhenLeader(ctx, func(context.Context) error { t.Error("follower invoked leader work"); return nil })
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower observer did not drain")
	}
	want := context.Canceled
	if err := leader.leadership.WhenLeader(context.Background(), func(context.Context) error { return want }); err != want {
		t.Fatalf("callback error=%v", err)
	}
	canceled, cancelImmediately := context.WithCancel(context.Background())
	cancelImmediately()
	if err := leader.leadership.WhenLeader(canceled, func(context.Context) error { t.Error("canceled caller invoked work"); return nil }); err != nil {
		t.Fatal(err)
	}
}
