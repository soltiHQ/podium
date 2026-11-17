package raft_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/event"
	raftpkg "github.com/soltiHQ/control-plane/internal/raft"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// TestFSM_SnapshotCarriesWorkloadSchemas persists and restores a snapshot of a
// store that holds a schema: the catalog survives the compaction.
func TestFSM_SnapshotCarriesWorkloadSchemas(t *testing.T) {
	ctx := context.Background()
	src := inmemory.New()
	w, err := model.NewWorkloadSchema(strings.Repeat("d", 64), "solti.io/v1", "Subprocess", []byte(`{"type":"object"}`), "agent-1", time.Unix(5000, 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := src.UpsertWorkloadSchema(ctx, w); err != nil {
		t.Fatal(err)
	}

	snap, err := raftpkg.NewFSM(src, event.NewHub(zerolog.Nop()), zerolog.Nop()).Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer snap.Release()
	sink := newBufSink()
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	dst := inmemory.New()
	if err := raftpkg.NewFSM(dst, event.NewHub(zerolog.Nop()), zerolog.Nop()).Restore(sink.reader()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := dst.GetWorkloadSchema(ctx, w.ID())
	if err != nil || got.Kind() != "Subprocess" || got.FirstAgentID() != "agent-1" || !got.FirstSeenAt().Equal(w.FirstSeenAt()) || !bytes.Equal(got.Schema(), w.Schema()) {
		t.Fatalf("restored schema: %+v, %v", got, err)
	}
}
