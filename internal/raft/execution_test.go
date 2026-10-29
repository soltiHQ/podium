package raft

import (
	"context"
	"errors"
	"testing"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func raftArchiveRow(t *testing.T, attempt uint32) *model.ExecutionRecord {
	t.Helper()
	code := int32(-9)
	e, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a", AgentName: "Agent", TaskName: "task", TaskUID: "uid", Generation: 2, Attempt: attempt, Status: "canceled", ExitCode: &code, Error: "canceled", CapturedAt: time.Unix(1000+int64(attempt), 0)})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestCommandArchiveVersionCompatibility(t *testing.T) {
	e := raftArchiveRow(t, 1)
	data, err := encodeCommand([]*raftv1.Op{{Op: &raftv1.Op_ExecutionUpsert{ExecutionUpsert: wire.ExecutionToProto(e)}}})
	if err != nil {
		t.Fatal(err)
	}
	if data[len(commandMagic)] != 3 {
		t.Fatal("newcommands lack downgrade guard")
	}
	if _, err := decodeCommand(data); err != nil {
		t.Fatal(err)
	}
	data[len(commandMagic)] = 2
	if _, err := decodeCommand(data); err == nil {
		t.Fatal("v2 newentity allowed")
	}
	old, _ := encodeCommand([]*raftv1.Op{{Op: &raftv1.Op_AgentDelete{AgentDelete: "a"}}})
	old[len(commandMagic)] = 2
	if _, err := decodeCommand(old); err != nil {
		t.Fatal("compatible v2 old ops rejected", err)
	}
	unknown := protowire.AppendTag(nil, 99, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, []byte("future"))
	op := &raftv1.Op{}
	op.ProtoReflect().SetUnknown(unknown)
	bad, _ := encodeCommand([]*raftv1.Op{op})
	if _, err := decodeCommand(bad); err == nil {
		t.Fatal("unknown entity accepted")
	}
}

func TestArchiveTransactionOverlaySupportsAtomicPruning(t *testing.T) {
	ctx := context.Background()
	inner := inmemory.New()
	first, second := raftArchiveRow(t, 1), raftArchiveRow(t, 2)
	if err := inner.UpsertExecution(ctx, first); err != nil {
		t.Fatal(err)
	}
	view := newTxView(inner)
	if err := view.UpsertExecution(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := view.DeleteExecution(ctx, first.ID()); err != nil {
		t.Fatal(err)
	}
	rows, err := storage.AllExecutions(ctx, view, view.BuildExecutionFilter(storage.ExecutionQueryCriteria{AgentID: "a"}))
	if err != nil || len(rows) != 1 || rows[0].ID() != second.ID() {
		t.Fatal("pruning listing ignores writes", err)
	}
	if _, err := inner.GetExecution(ctx, second.ID()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("recording transaction mutated store", err)
	}
	got, err := view.GetExecution(ctx, second.ID())
	if err != nil {
		t.Fatal(err)
	}
	p := got.Params()
	*p.ExitCode = 77
	if *second.ExitCode() != -9 {
		t.Fatal("overlay aliased input")
	}
}

func TestRaftExecutionArchiveRestoresLogAndSnapshot(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "log", true: "snapshot"}[compact], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			p, _, hub := openRestartProfile(t, dir)
			e := raftArchiveRow(t, 1)
			if err := p.Store().WithTx(ctx, func(tx storage.Storage) error {
				if err := tx.UpsertExecution(ctx, e); err != nil {
					return err
				}
				rows, err := storage.AllExecutions(ctx, tx, nil)
				if err != nil || len(rows) != 1 {
					t.Fatal("archive batch listing", err)
				}
				return nil
			}); err != nil {
				_ = p.Shutdown()
				hub.Close()
				t.Fatal(err)
			}
			if compact {
				if err := p.raft.Snapshot().Error(); err != nil {
					_ = p.Shutdown()
					hub.Close()
					t.Fatal(err)
				}
			}
			if err := p.Shutdown(); err != nil {
				hub.Close()
				t.Fatal(err)
			}
			hub.Close()
			restored, inner, rhub := openRestartProfile(t, dir)
			defer func() { _ = restored.Shutdown() }()
			defer rhub.Close()
			if err := cluster.AwaitLeaderReady(ctx, restored.Leadership()); err != nil {
				t.Fatal(err)
			}
			got, err := inner.GetExecution(ctx, e.ID())
			if err != nil || !proto.Equal(wire.ExecutionToProto(e), wire.ExecutionToProto(got)) {
				t.Fatal("history not restored", err)
			}
		})
	}
}
