package raft

import (
	"context"
	"fmt"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/domain/model"
)

// The compaction settings of Config reach hashicorp/raft, and zero keeps its
// defaults. With them a node snapshots on its own once enough entries pile up
// and then truncates its log down to the trailing logs: neither the default
// threshold of 8192 entries, the default interval of 120 s nor the default of
// 10240 trailing logs would do either within this test.
func TestCompactionKnobsApplied(t *testing.T) {
	type compaction struct {
		interval            time.Duration
		threshold, trailing uint64
	}
	t.Run("config", func(t *testing.T) {
		var got compaction
		startRaft := newRaft
		t.Cleanup(func() { newRaft = startRaft })
		newRaft = func(conf *hraft.Config, fsm hraft.FSM, logs hraft.LogStore, stable hraft.StableStore, snaps hraft.SnapshotStore, trans hraft.Transport) (*hraft.Raft, error) {
			got = compaction{conf.SnapshotInterval, conf.SnapshotThreshold, conf.TrailingLogs}
			return startRaft(conf, fsm, logs, stable, snaps, trans)
		}
		for _, tc := range []struct {
			name string
			opt  func(*Config)
			want compaction
		}{
			{"zero keeps the defaults", func(*Config) {}, compaction{120 * time.Second, 8192, 10240}},
			{"set", func(c *Config) {
				c.SnapshotInterval, c.SnapshotThreshold, c.TrailingLogs = 30*time.Second, 100, 50
			}, compaction{30 * time.Second, 100, 50}},
			{"set one", func(c *Config) { c.TrailingLogs = 7 }, compaction{120 * time.Second, 8192, 7}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				n := newLoopbackNode(t, newTestCA(t), "n1", reserveLoopbackAddrs(t, 1)[0])
				t.Cleanup(n.stop)
				n.start(t, n.self(), oneNodeTimeouts, tc.opt)
				if got != tc.want {
					t.Fatalf("hashicorp/raft got %+v, want %+v", got, tc.want)
				}
			})
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		const threshold, trailing = 10, 4
		n := newLoopbackNode(t, newTestCA(t), "n1", reserveLoopbackAddrs(t, 1)[0])
		t.Cleanup(n.stop)
		n.start(t, n.self(), oneNodeTimeouts, func(c *Config) {
			c.SnapshotInterval, c.SnapshotThreshold, c.TrailingLogs = 25*time.Millisecond, threshold, trailing
		})
		waitFor(t, 5*time.Second, "n1 leads", n.profile.leadership.AmLeader)

		ctx := context.Background()
		for i := range 3 * threshold {
			agent, err := model.NewAgent(fmt.Sprintf("a%d", i), "agent", "http://agent")
			if err != nil {
				t.Fatal(err)
			}
			if err := n.profile.Store().UpsertAgent(ctx, agent); err != nil {
				t.Fatal(err)
			}
		}
		logs := n.profile.logStore
		waitFor(t, 10*time.Second, "a snapshot truncates the log", func() bool {
			first, err := logs.FirstIndex()
			return err == nil && first > 1
		})

		// Every entry is applied, so a snapshot now leaves exactly the trailing logs.
		if err := n.profile.raft.Snapshot().Error(); err != nil {
			t.Fatal(err)
		}
		first, err := logs.FirstIndex()
		if err != nil {
			t.Fatal(err)
		}
		last, err := logs.LastIndex()
		if err != nil {
			t.Fatal(err)
		}
		if kept := last - first + 1; kept != trailing {
			t.Fatalf("the log holds %d entries (%d to %d) after a snapshot, want the %d trailing logs", kept, first, last, trailing)
		}
	})
}
