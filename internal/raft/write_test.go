package raft

import (
	"context"
	"errors"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// stubFuture is the future of a write that answers err, with index as the
// index raft dispatched the write at, 0 for one it never dispatched. A stub
// with an answer channel answers only once the channel is closed.
type stubFuture struct {
	index  uint64
	err    error
	answer chan struct{}
}

func (f *stubFuture) Error() error {
	if f.answer != nil {
		<-f.answer
	}
	return f.err
}

func (f *stubFuture) Index() uint64 { return f.index }
func (f *stubFuture) Response() any { return nil }

// proposeStub makes the store of p propose every write as future, and returns
// a channel that receives once for each write the store proposes.
func proposeStub(t *testing.T, p *Profile, future *stubFuture) <-chan struct{} {
	t.Helper()
	proposed := make(chan struct{}, 16)
	p.store.propose = func([]byte, time.Duration) hraft.ApplyFuture {
		proposed <- struct{}{}
		return future
	}
	return proposed
}

// A write whose future raft answers with ErrRaftShutdown after the leader
// dispatched it, at an index, may have committed: hashicorp/raft v1.8.0 answers
// so a committed write whose hand-over to the FSM met the shutdown. Its caller
// gets cluster.ErrLeadershipLost, an unknown outcome, and still the error of
// hashicorp/raft. The same error for a write raft never dispatched, with no
// index, and ErrEnqueueTimeout, wrote nothing: storage.ErrUnavailable. A
// membership change has the same meanings, and keeps its conflict.
func TestWriteOutcomeOfARaftShutdown(t *testing.T) {
	changed := errors.New("configuration changed since 3 (latest is 4)")
	for _, tc := range []struct {
		name   string
		future *stubFuture
		want   error // the meaning, besides the error of the future
		not    error // a meaning the error must not have
	}{
		{"dispatched, then shut down", &stubFuture{index: 7, err: hraft.ErrRaftShutdown}, cluster.ErrLeadershipLost, storage.ErrUnavailable},
		{"never dispatched", &stubFuture{err: hraft.ErrRaftShutdown}, storage.ErrUnavailable, cluster.ErrLeadershipLost},
		{"enqueue timeout", &stubFuture{err: hraft.ErrEnqueueTimeout}, storage.ErrUnavailable, cluster.ErrLeadershipLost},
		{"leadership lost", &stubFuture{index: 7, err: hraft.ErrLeadershipLost}, cluster.ErrLeadershipLost, storage.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSingleNode(t)
			proposeStub(t, p, tc.future)
			agent := testAgent(t, "a1")
			err := p.Store().UpsertAgent(context.Background(), agent)
			if !errors.Is(err, tc.want) || !errors.Is(err, tc.future.err) || errors.Is(err, tc.not) {
				t.Fatalf("UpsertAgent = %v, want %v, not %v, and still %v", err, tc.want, tc.not, tc.future.err)
			}
			if kind := errkind.Classify(err); kind != errkind.Unavailable {
				t.Fatalf("errkind.Classify(%v) = %v, want Unavailable", err, kind)
			}
			if err := membershipError(tc.future, tc.future.err); !errors.Is(err, tc.want) || errors.Is(err, tc.not) {
				t.Fatalf("membershipError = %v, want %v, not %v", err, tc.want, tc.not)
			}
		})
	}
	if err := membershipError(&stubFuture{err: changed}, changed); !errors.Is(err, cluster.ErrConfigurationChanged) {
		t.Fatalf("membershipError(%v) = %v, want %v", changed, err, cluster.ErrConfigurationChanged)
	}
}

// A write whose future never answers, as hashicorp/raft v1.8.0 can leave a
// committed write at shutdown, ends once its node shuts down: its caller gets
// cluster.ErrLeadershipLost, an unknown outcome, and the store's txMu is free
// again. The same holds for the wait of awaitWrite alone, which the event
// writer and the membership changes share.
func TestAWriteNeverAnsweredEndsWithItsNode(t *testing.T) {
	p := newSingleNode(t)
	hung := &stubFuture{index: 7, answer: make(chan struct{})}
	t.Cleanup(func() { close(hung.answer) }) // ends the wait the store left behind
	proposed := proposeStub(t, p, hung)

	written := make(chan error, 1)
	agent := testAgent(t, "a1")
	go func() { written <- p.Store().UpsertAgent(context.Background(), agent) }()
	select {
	case <-proposed:
	case err := <-written:
		t.Fatalf("UpsertAgent = %v before its write was proposed", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the write was not proposed within 5s")
	}
	if err := p.raft.Shutdown().Error(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-written:
		if !errors.Is(err, cluster.ErrLeadershipLost) || !errors.Is(err, errInterrupted) {
			t.Fatalf("UpsertAgent = %v, want %v once the node shut down", err, cluster.ErrLeadershipLost)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write still waits 5s after its node shut down")
	}
	if !p.store.txMu.TryLock() {
		t.Fatal("the store's txMu is still held once the write returned")
	}
	p.store.txMu.Unlock()

	stopped := make(chan struct{})
	close(stopped)
	never := &stubFuture{index: 9, answer: make(chan struct{})}
	t.Cleanup(func() { close(never.answer) })
	if err := writeError(never, awaitWrite(never, stopped)); !errors.Is(err, cluster.ErrLeadershipLost) {
		t.Fatalf("awaitWrite of an unanswered future on a stopped node: %v, want %v", err, cluster.ErrLeadershipLost)
	}
}

// The signal of a raft node's shutdown is closed however the node shuts down,
// and at once for a node that shut down already.
func TestStopSignalFollowsTheShutdown(t *testing.T) {
	p := newSingleNode(t)
	signal := stopSignal(p.raft)
	select {
	case <-signal:
		t.Fatal("the signal is closed while the node runs")
	default:
	}
	if err := p.raft.Shutdown().Error(); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]<-chan struct{}{"registered before": signal, "registered after": stopSignal(p.raft)} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("the signal %s the shutdown is still open 5s after it", name)
		}
	}
}
