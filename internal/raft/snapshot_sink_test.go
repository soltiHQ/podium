package raft_test

import (
	"errors"
	"testing"

	raftpkg "github.com/soltiHQ/control-plane/internal/raft"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type failingSink struct {
	short     bool
	closeErr  error
	cancelled bool
}

func (s *failingSink) ID() string { return "failing" }
func (s *failingSink) Write(p []byte) (int, error) {
	if s.short {
		return len(p) - 1, nil
	}
	return len(p), nil
}
func (s *failingSink) Close() error  { return s.closeErr }
func (s *failingSink) Cancel() error { s.cancelled = true; return nil }

func TestSnapshotCancelsShortWriteAndCloseFailure(t *testing.T) {
	for _, sink := range []*failingSink{{short: true}, {closeErr: errors.New("close failed")}} {
		snap, err := raftpkg.NewFSM(inmemory.New(), nil).Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if err := snap.Persist(sink); err == nil {
			t.Fatal("sink failure not surfaced")
		}
		if !sink.cancelled {
			t.Fatal("failed snapshot not cancelled")
		}
		snap.Release()
	}
}
