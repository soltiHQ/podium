package wire_test

import (
	"testing"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/wire"
	"google.golang.org/protobuf/proto"
)

func TestRolloutRecoveryPreservesEveryObservedField(t *testing.T) {
	for _, status := range []enum.SyncStatus{enum.SyncStatusPending, enum.SyncStatusSynced, enum.SyncStatusDrift, enum.SyncStatusFailed, enum.SyncStatusUnknown} {
		original := &raftv1.RolloutMsg{Id: "rid-spec-agent", SpecId: "spec", AgentId: "agent", ActualTaskId: "resource", DesiredGeneration: 8, ObservedGeneration: 6, Attempts: 4, Status: uint32(status), Intent: uint32(enum.RolloutIntentUninstall), ErrMsg: "last failure", CreatedAtNs: 123456789, UpdatedAtNs: 234567891, LastPushedAtNs: 345678912, LastSyncedAtNs: 456789123}
		restored, err := wire.RolloutFromProto(original)
		if err != nil {
			t.Fatal(err)
		}
		if back := wire.RolloutToProto(restored); !proto.Equal(original, back) {
			t.Fatalf("status %s recovery changed fields: before=%v after=%v", status, original, back)
		}
	}
}
