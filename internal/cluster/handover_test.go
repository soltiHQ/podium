package cluster_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// movingView is a follower's view of the leader that changes when the test says.
type movingView struct {
	mu      sync.Mutex
	leading bool
	leader  string
}

func (v *movingView) AmLeader() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.leading
}

func (v *movingView) CurrentLeader() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.leader
}

func (*movingView) WhenLeader(ctx context.Context, _ func(context.Context) error) error {
	<-ctx.Done()
	return nil
}

// set changes the view after delay.
func (v *movingView) set(delay time.Duration, leading bool, leader string) {
	time.AfterFunc(delay, func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.leading, v.leader = leading, leader
	})
}

// AwaitMove reports where the view of the leader went once it moved off the
// leader a forward could not reach: to this node, or to another leader, at
// once when it has moved already. A view that stays, or that knows no leader,
// reports nothing once the wait is over, and a client that gives up ends the
// wait. A wait of zero or less looks once.
func TestAwaitMove(t *testing.T) {
	const old, next = "[10.0.0.1]:8080", "[10.0.0.2]:8080"
	for _, tc := range []struct {
		name        string
		start       string // the view's leader at the start
		moveAfter   time.Duration
		leading     bool   // the view leads after the move
		to          string // the view's leader after the move
		wait        time.Duration
		cancelAfter time.Duration // the client gives up after this, when set
		wantLeading bool
		wantLeader  string
		minTook     time.Duration
		maxTook     time.Duration
	}{
		{"moved already", next, 0, false, next, time.Second, 0, false, next, 0, 200 * time.Millisecond},
		{"moves to another leader", old, 100 * time.Millisecond, false, next, 2 * time.Second, 0, false, next, 100 * time.Millisecond, time.Second},
		{"moves to this node", old, 100 * time.Millisecond, true, "", 2 * time.Second, 0, true, "", 100 * time.Millisecond, time.Second},
		{"stays", old, 0, false, old, 300 * time.Millisecond, 0, false, "", 300 * time.Millisecond, 1500 * time.Millisecond},
		{"knows no leader", old, 50 * time.Millisecond, false, "", 300 * time.Millisecond, 0, false, "", 300 * time.Millisecond, 1500 * time.Millisecond},
		{"the client gives up", old, 0, false, old, 5 * time.Second, 100 * time.Millisecond, false, "", 100 * time.Millisecond, time.Second},
		{"no wait, moved already", next, 0, false, next, 0, 0, false, next, 0, 200 * time.Millisecond},
		{"no wait, stays", old, 0, false, old, -1, 0, false, "", 0, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := &movingView{leader: tc.start}
			if tc.moveAfter > 0 {
				view.set(tc.moveAfter, tc.leading, tc.to)
			}
			ctx := context.Background()
			if tc.cancelAfter > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.cancelAfter)
				defer cancel()
			}
			start := time.Now()
			leading, leader := cluster.AwaitMove(ctx, view, old, tc.wait)
			took := time.Since(start)
			if leading != tc.wantLeading || leader != tc.wantLeader {
				t.Fatalf("AwaitMove() = (%v, %q), want (%v, %q)", leading, leader, tc.wantLeading, tc.wantLeader)
			}
			if took < tc.minTook || took > tc.maxTook {
				t.Fatalf("AwaitMove() took %s, want %s to %s", took, tc.minTook, tc.maxTook)
			}
		})
	}
}
