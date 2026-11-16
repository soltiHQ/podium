package cluster

import (
	"context"
	"time"
)

// The forwards of a follower to the leader, over HTTP and gRPC alike, across a
// handover. A leader that hands over, as its pod stops at a rollout, releases
// its lease, raft's or the database's, and exits; a follower learns of the next
// leader within its next look at the lease, a second at most in the database
// mode. In between it still sends writes to the leader that left: once that
// leader's listeners close they refuse the connection, and once its pod is gone
// nothing answers at its address at all. A forward that fails to connect sent
// nothing of the write, so the follower waits for its view of the leader to
// move (AwaitMove), and then serves the write itself, as the next leader, or
// sends it to the next leader once more, instead of answering 502. A write
// that reaches a follower while it knows no leader, between a release and the
// takeover, waits for a leader the same way.
//
// The wait assumes that the leader that left releases its lease soon after its
// listeners close. It releases it once they have drained, within
// server.shutdown_timeout, and once its leader-only work has returned: a drain
// that takes longer than HandoverWait leaves the writes that met the closed
// listeners refused, 502 over HTTP, as before.
const (
	// ForwardDialTimeout bounds the connect of a forward to the leader. Within a
	// cluster a connect takes milliseconds: a leader that has not answered within
	// it is gone, and the forward stops waiting for it. It leaves room for one
	// lost SYN, which Linux sends again after a second.
	ForwardDialTimeout = 2 * time.Second

	// HandoverWait is how long a follower waits, once a forward failed to
	// connect, for its view of the leader to move: two looks at the database
	// mode's lease, which a handover takes, and the takeover. A leader that
	// crashed holds its lease until it expires, ten seconds by default, which a
	// write does not wait for.
	HandoverWait = 3 * time.Second
)

// handoverPoll is how often AwaitMove looks at the view of the leader.
const handoverPoll = 50 * time.Millisecond

// AwaitMove waits for leadership's view of the leader to move off from, the
// address of the leader that a forward could not connect to, as
// [Leadership.CurrentLeader] gave it. It looks at once, and then every
// handoverPoll until wait is over or ctx ends, and reports where the view
// went: leading when this node leads, or the address of the other leader it
// knows. It reports neither while the view stays on from or knows no leader:
// the forward then fails as it did. A wait of zero or less looks once.
func AwaitMove(ctx context.Context, leadership Leadership, from string, wait time.Duration) (leading bool, leader string) {
	var deadline <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C
	}
	tick := time.NewTicker(handoverPoll)
	defer tick.Stop()
	for {
		if leadership.AmLeader() {
			return true, ""
		}
		if current := leadership.CurrentLeader(); current != "" && current != from {
			return false, current
		}
		if deadline == nil {
			return false, ""
		}
		select {
		case <-tick.C:
		case <-deadline:
			return false, ""
		case <-ctx.Done():
			return false, ""
		}
	}
}
