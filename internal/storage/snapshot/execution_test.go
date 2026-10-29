package snapshot_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	"google.golang.org/protobuf/proto"
)

func TestSnapshotExecutionRoundtripAndVersionGuard(t *testing.T) {
	code := int32(0)
	e, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "a", TaskName: "task", TaskUID: "uid", Generation: 1, Attempt: 1, Status: "succeeded", ExitCode: &code, CapturedAt: time.Unix(1000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := snapshot.Marshal(inmemory.SnapshotContent{Executions: []*model.ExecutionRecord{e}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := snapshot.Unmarshal(data)
	if err != nil || len(out.Executions) != 1 || !reflect.DeepEqual(out.Executions[0].Params(), e.Params()) {
		t.Fatal("roundtrip lost fields", err)
	}
	for _, change := range []func(*raftv1.Snapshot){func(m *raftv1.Snapshot) { m.Header.Version = 2 }, func(m *raftv1.Snapshot) { m.Executions[0].Id = "forged" }, func(m *raftv1.Snapshot) { m.Executions[0].Status = "running" }, func(m *raftv1.Snapshot) { m.Executions = append(m.Executions, m.Executions[0]) }, func(m *raftv1.Snapshot) { m.Executions[0].Error = strings.Repeat("x", 32769) }} {
		var m raftv1.Snapshot
		if err := proto.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		change(&m)
		bad, _ := proto.Marshal(&m)
		if _, err := snapshot.Unmarshal(bad); err == nil {
			t.Fatal("invalid archive state accepted")
		}
	}
	nilExit := e.Params()
	nilExit.ExitCode = nil
	noCode, _ := model.NewExecutionRecord(nilExit)
	if wire.ExecutionToProto(noCode).ExitCode != nil || wire.ExecutionToProto(e).ExitCode == nil {
		t.Fatal("nil vs zero exitcode lost")
	}
}
