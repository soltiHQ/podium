package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

func TestExecutionRESTKeepsFirstCaptureSDKIdentityAndZeroExit(t *testing.T) {
	exit := int32(0)
	captured := time.Date(2026, 10, 2, 3, 0, 0, 123456789, time.UTC)
	record, err := model.NewExecutionRecord(model.ExecutionRecordParams{AgentID: "removed", TaskName: "same-name", TaskUID: "OriginalUID", Generation: 99, Attempt: 3, Status: "succeeded", ExitCode: &exit, CapturedAt: captured, SourceResourceVersion: "opaque/revision", WorkloadAPIVersion: "solti.io/v1", WorkloadKind: "Subprocess"})
	if err != nil {
		t.Fatal(err)
	}
	dto := ExecutionToREST(record)
	if dto.TaskUID != "OriginalUID" || dto.Generation != 99 || dto.Attempt != 3 || dto.ExitCode == nil || *dto.ExitCode != 0 || dto.SourceResourceVersion != "opaque/revision" || dto.CapturedAt != "2026-10-02T03:00:00.123456789Z" {
		t.Fatalf("mapping lost observation: %+v", dto)
	}
	*dto.ExitCode = 8
	if *record.ExitCode() != 0 {
		t.Fatal("DTO aliases stored result")
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"command"`, `"env"`, `"logs"`, `"manifest"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("archive DTO exposes %s", forbidden)
		}
	}
}
