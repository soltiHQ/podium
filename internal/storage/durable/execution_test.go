package durable

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
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

// A single-blob file of snapshot version 3 migrates to a version-4 snapshot with
// its ten groups unchanged, its stored credential stored still; a derived
// credential then joins the log, and the next snapshot.
func TestDurableSnapshotV3UpgradePreservesTenGroups(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	populate(t, s)
	if err := s.UpsertExecution(ctx, durableArchiveRow(t)); err != nil {
		t.Fatal(err)
	}
	data, err := snapshot.Marshal(s.inner.SnapshotForRaft())
	if err != nil {
		t.Fatal(err)
	}
	var msg raftv1.Snapshot
	if err := proto.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	msg.Header.Version = 3
	data, err = proto.MarshalOptions{Deterministic: true}.Marshal(&msg)
	if err != nil {
		t.Fatal(err)
	}
	dir := singleBlobFile(t, data)
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := snapshot.Marshal(again.inner.SnapshotForRaft())
	if err != nil {
		t.Fatal(err)
	}
	var got raftv1.Snapshot
	if err := proto.Unmarshal(reencoded, &got); err != nil {
		t.Fatal(err)
	}
	msg.Header.Version = 4
	if !proto.Equal(&msg, &got) {
		t.Fatal("v3 migration changed existing entity fields")
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	var migrated raftv1.Snapshot
	if err := proto.Unmarshal(readLayout(t, dir).blob, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.GetHeader().GetVersion() != 4 || len(migrated.GetExecutions()) != 1 || len(migrated.GetAgentCredentials()) != 1 ||
		migrated.GetAgentCredentials()[0].Generation != nil || migrated.GetAgentCredentials()[0].GetToken() != "test-enrollment-token" {
		t.Fatalf("the snapshot right after the migration is version %d with %d records and credentials %v, want version 4 with the record and the stored credential",
			migrated.GetHeader().GetVersion(), len(migrated.GetExecutions()), migrated.GetAgentCredentials())
	}
	again, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.New(again, zerolog.Nop(), agent.WithEnrollSecret(credentialSecret)).VerifyOrEnrollToken(ctx, "a3", agent.DeriveToken(credentialSecret, "a3", 0)); err != nil {
		t.Fatal(err)
	}
	if err := again.writeSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	var saved raftv1.Snapshot
	if err := proto.Unmarshal(readLayout(t, dir).blob, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.GetHeader().GetVersion() != 4 || len(saved.GetAgentCredentials()) != 2 || saved.GetAgentCredentials()[1].Generation == nil {
		t.Fatal("the snapshot after the derived credential is not version 4 with it")
	}
}
