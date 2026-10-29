package wire

import (
	"fmt"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

func ExecutionToProto(e *model.ExecutionRecord) *raftv1.ExecutionRecordMsg {
	return &raftv1.ExecutionRecordMsg{Id: e.ID(), AgentId: e.AgentID(), AgentName: e.AgentName(), TaskName: e.TaskName(), TaskUid: e.TaskUID(), Generation: e.Generation(), Attempt: e.Attempt(), Status: e.Status(), StartedAtMs: e.StartedAtMs(), FinishedAtMs: e.FinishedAtMs(), ExitCode: e.ExitCode(), Error: e.Error(), WorkloadApiVersion: e.WorkloadAPIVersion(), WorkloadKind: e.WorkloadKind(), SpecId: e.SpecID(), SpecName: e.SpecName(), SourceResourceVersion: e.SourceResourceVersion(), CapturedAtNs: e.CapturedAt().UnixNano()}
}

func ExecutionFromProto(m *raftv1.ExecutionRecordMsg) (*model.ExecutionRecord, error) {
	if m == nil {
		return nil, fmt.Errorf("execution record: missing message")
	}
	e, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: m.GetAgentId(), AgentName: m.GetAgentName(), TaskName: m.GetTaskName(), TaskUID: m.GetTaskUid(), Generation: m.GetGeneration(), Attempt: m.GetAttempt(), Status: m.GetStatus(), StartedAtMs: m.GetStartedAtMs(), FinishedAtMs: m.GetFinishedAtMs(), ExitCode: m.ExitCode, Error: m.GetError(), WorkloadAPIVersion: m.GetWorkloadApiVersion(), WorkloadKind: m.GetWorkloadKind(), SpecID: m.GetSpecId(), SpecName: m.GetSpecName(), SourceResourceVersion: m.GetSourceResourceVersion(), CapturedAt: time.Unix(0, m.GetCapturedAtNs())})
	if err != nil {
		return nil, err
	}
	if m.GetId() != e.ID() {
		return nil, fmt.Errorf("execution record: identity mismatch")
	}
	return e, nil
}
