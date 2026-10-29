package raft

import (
	"fmt"
	"io"

	hraft "github.com/hashicorp/raft"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// fsmSnapshot captures decoupled state so Persist may run concurrently with Apply.
type fsmSnapshot struct{ content inmemory.SnapshotContent }

func (s *fsmSnapshot) Persist(sink hraft.SnapshotSink) error {
	data, err := snapshot.Marshal(s.content)
	if err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("raft snapshot: %w", err)
	}
	n, err := sink.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("raft snapshot: write: %w", err)
	}
	if err := sink.Close(); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("raft snapshot: close: %w", err)
	}
	return nil
}

func (s *fsmSnapshot) Release() {}
