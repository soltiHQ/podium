package raft

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/storagecontract"
)

// TestContract runs the storage contract on the raft store of a one-node
// cluster, so the suite pins what every backend owes services and not what the
// in-memory store happens to do. The cluster starts once; each check gets it
// emptied by restoring the snapshot the empty node itself took.
func TestContract(t *testing.T) {
	leader := newLoopbackCluster(t, "n1").leader(t, 10*time.Second)
	node := leader.profile.raft

	// A snapshot needs an entry the FSM applied, and a barrier is one:
	// hashicorp/raft hands the FSM neither the new leader's no-op nor, podium's
	// FSM being no ConfigurationStore, the configuration.
	if err := node.Barrier(10 * time.Second).Error(); err != nil {
		t.Fatalf("barrier on the empty store: %v", err)
	}
	future := node.Snapshot()
	if err := future.Error(); err != nil {
		t.Fatalf("snapshot of the empty store: %v", err)
	}
	meta, rc, err := future.Open()
	if err != nil {
		t.Fatalf("open the snapshot: %v", err)
	}
	empty, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}

	storagecontract.Run(t, func(t *testing.T) storage.Storage {
		if err := node.Restore(meta, bytes.NewReader(empty), 10*time.Second); err != nil {
			t.Fatalf("empty the store: %v", err)
		}
		return leader.profile.Store()
	})
}
