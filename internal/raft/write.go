package raft

import (
	"errors"
	"sync"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// errInterrupted is what awaitWrite gives a write whose node shut down before
// the write's future answered.
var errInterrupted = errors.New("raft shut down before the outcome of the write was known")

// stopSignal returns a channel that is closed once r shuts down, however it
// does: Profile.Shutdown, or hashicorp/raft by itself, as a leader that commits
// its own removal does with ShutdownOnRemove. hashicorp/raft v1.8.0 hands out
// no channel of its shutdown, so the signal is an observer of its state, which
// Shutdown sets to Shutdown as it closes its own channel, before it waits for
// its goroutines.
func stopSignal(r *hraft.Raft) <-chan struct{} {
	stopped := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopped) }) }
	r.RegisterObserver(hraft.NewObserver(nil, false, func(o *hraft.Observation) bool {
		if state, ok := o.Data.(hraft.RaftState); ok && state == hraft.Shutdown {
			stop()
		}
		return false
	}))
	// A raft that shut down before the observer was registered observes nothing
	// more.
	if r.State() == hraft.Shutdown {
		stop()
	}
	return stopped
}

// refuseStopped returns, for a node whose raft shut down already, the error of
// a write that raft never took: storage.ErrUnavailable, and still
// hashicorp/raft's ErrRaftShutdown (clusterError). Such a write must not be
// proposed: raft would refuse it unwritten, and awaitWrite, which finds the
// stop signal closed, would take that refusal for an unknown outcome. It
// returns nil while the node runs; a shutdown that comes after it leaves the
// write to awaitWrite and writeError.
func refuseStopped(stopped <-chan struct{}) error {
	select {
	case <-stopped:
		return clusterError(hraft.ErrRaftShutdown)
	default:
		return nil
	}
}

// awaitWrite waits for f, the future of a write that raft took, and returns its
// error, but no longer than the node runs: once stopped is closed, a future
// that has not answered gives errInterrupted, an unknown outcome, which is the
// safe answer for a write whose answer comes as the node shuts down too.
// hashicorp/raft v1.8.0 can leave the future of a committed write unanswered
// for ever: at shutdown its FSM goroutine returns without draining the writes
// it was handed (runFSM), and the future of a write waits for no shutdown
// (ApplyLog sets none). The wait for such a future goes on in a goroutine of
// its own, which ends if the future ever answers, and the caller, who may hold
// a Store's txMu, returns.
func awaitWrite(f hraft.Future, stopped <-chan struct{}) error {
	answer := make(chan error, 1)
	go func() { answer <- f.Error() }()
	select {
	case err := <-answer:
		return err
	case <-stopped:
		// An answer that is there as the shutdown is seen is the write's own.
		select {
		case err := <-answer:
			return err
		default:
			return errInterrupted
		}
	}
}

// writeError is clusterError for err, what awaitWrite gave the write whose
// future is f. A write of unknown outcome is [cluster.ErrLeadershipLost], as it
// may have committed:
//
//   - one whose node shut down before its future answered (errInterrupted);
//   - one that the leader dispatched, appending it to its log, which gave it an
//     index, and that raft answered with ErrRaftShutdown: hashicorp/raft
//     v1.8.0 answers so a committed write whose hand-over to the FSM met the
//     shutdown (processLogs).
//
// ErrRaftShutdown of a write that raft never dispatched, whose future has no
// index, and ErrEnqueueTimeout, which no dispatched write gets, stay
// storage.ErrUnavailable (clusterError): raft wrote nothing. A write whose
// node had shut down before it was proposed is refused before raft sees it
// (refuseStopped), with the same storage.ErrUnavailable; one proposed as the
// node shuts down can still end as errInterrupted, the safe side.
func writeError(f hraft.IndexFuture, err error) error {
	switch {
	case errors.Is(err, errInterrupted):
		return meant{meaning: cluster.ErrLeadershipLost, cause: err}
	case errors.Is(err, hraft.ErrRaftShutdown) && f.Index() > 0:
		return meant{meaning: cluster.ErrLeadershipLost, cause: err}
	}
	return clusterError(err)
}
