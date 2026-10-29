package durable

import (
	"context"
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestDurableExecutionCommitErrorDoesNotPublish(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()
	e := durableArchiveRow(t)
	// A closed bbolt handle deterministically fails the disk commit; this is
	// commit-path fault injection, not a claim of real fsync/power-loss testing.
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertExecution(ctx, e); err == nil {
		t.Fatal("failed disk commit acknowledged")
	}
	if _, err := s.inner.GetExecution(ctx, e.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("failed archive candidate published", err)
	}
	if _, err := s.GetExecution(ctx, e.ID()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal("commit failure did not fail closed", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.GetExecution(ctx, e.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("unacknowledged archive restored", err)
	}
}
