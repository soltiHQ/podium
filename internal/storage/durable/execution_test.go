package durable

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

func durableArchiveRow(t *testing.T) *model.ExecutionRecord {
	t.Helper()
	code := int32(23)
	e, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a1", AgentName: "Agent", TaskName: "task", TaskUID: "uid", Generation: 2, Attempt: 3, Status: "exhausted", StartedAtMs: 100, FinishedAtMs: 90, ExitCode: &code, Error: "bounded error", WorkloadAPIVersion: "solti.io/v1", WorkloadKind: "Subprocess", SpecID: "sp1", SpecName: "Spec", SourceResourceVersion: "17", CapturedAt: time.Unix(2000, 10)})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDurableExecutionRestartRollbackAndNoCascade(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()
	populate(t, s)
	e := durableArchiveRow(t)
	if err := s.UpsertExecution(ctx, e); err != nil {
		t.Fatal(err)
	}
	p := e.Params()
	p.CapturedAt = p.CapturedAt.Add(time.Hour)
	replay, _ := model.NewExecutionRecord(p)
	if err := s.UpsertExecution(ctx, replay); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("rollback")
	if err := s.WithTx(ctx, func(tx storage.Storage) error {
		if err := tx.DeleteExecution(ctx, e.ID()); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := s.DeleteAgent(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSpec(ctx, "sp1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	got, err := again.GetExecution(ctx, e.ID())
	if err != nil || !reflect.DeepEqual(got.Params(), e.Params()) {
		t.Fatal("archive not losslessly restored", err)
	}
	if err := again.DeleteExecution(ctx, e.ID()); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	last, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = last.Close() }()
	if _, err := last.GetExecution(ctx, e.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("deleted history resurrected", err)
	}
}

func TestDurableSnapshotV2UpgradePreservesNineGroups(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()
	populate(t, s)
	before := s.inner.SnapshotForRaft()
	data, err := snapshot.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var msg raftv1.Snapshot
	if err := proto.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	msg.Header.Version = 2
	data, err = proto.MarshalOptions{Deterministic: true}.Marshal(&msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error { return putSnapshot(tx.Bucket(stateBucket), data) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.inner.SnapshotForRaft().Executions) != 0 {
		t.Fatal("v2 invented history")
	}
	reencoded, err := snapshot.Marshal(again.inner.SnapshotForRaft())
	if err != nil {
		t.Fatal(err)
	}
	var got raftv1.Snapshot
	if err := proto.Unmarshal(reencoded, &got); err != nil {
		t.Fatal(err)
	}
	msg.Header.Version = 3
	if !proto.Equal(&msg, &got) {
		t.Fatal("v2 migration changed existing entity fields")
	}
	if err := again.UpsertExecution(ctx, durableArchiveRow(t)); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.View(func(tx *bolt.Tx) error {
		var saved raftv1.Snapshot
		if err := proto.Unmarshal(tx.Bucket(stateBucket).Get(snapshotKey), &saved); err != nil {
			return err
		}
		if saved.GetHeader().GetVersion() != 3 || len(saved.GetExecutions()) != 1 {
			t.Fatal("nextwrite did not upgrade")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
