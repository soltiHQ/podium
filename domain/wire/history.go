package wire

import (
	"time"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

// ExecutionToREST retains SDK identity and trusted first-capture provenance.
func ExecutionToREST(record *model.ExecutionRecord) restv1.ExecutionRecord {
	if record == nil {
		return restv1.ExecutionRecord{}
	}
	p := record.Params()
	return restv1.ExecutionRecord{
		ID: record.ID(), AgentID: p.AgentID, AgentName: p.AgentName, TaskName: p.TaskName, TaskUID: p.TaskUID,
		Generation: p.Generation, Attempt: p.Attempt, Status: p.Status, StartedAtMs: p.StartedAtMs, FinishedAtMs: p.FinishedAtMs,
		ExitCode: p.ExitCode, Error: p.Error, WorkloadAPIVersion: p.WorkloadAPIVersion, WorkloadKind: p.WorkloadKind,
		SpecID: p.SpecID, SpecName: p.SpecName, SourceResourceVersion: p.SourceResourceVersion, CapturedAt: p.CapturedAt.UTC().Format(time.RFC3339Nano),
	}
}
