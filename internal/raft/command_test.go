package raft

import (
	"strings"
	"testing"

	agentv1 "github.com/soltiHQ/control-plane/api/gen/solti/agent/v1"
	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"google.golang.org/protobuf/proto"
)

func TestCommandVersionGuardAndStructuredCapabilityRoundTrip(t *testing.T) {
	original := &raftv1.Command{Ops: []*raftv1.Op{{Op: &raftv1.Op_AgentUpsert{AgentUpsert: &raftv1.AgentMsg{Id: "agent", Capabilities: &agentv1.AgentCapabilities{Runners: []*agentv1.RunnerCapability{{Name: "default", Workloads: []*agentv1.WorkloadType{{ApiVersion: "solti.io/v1", Kind: "Subprocess"}}}}}}}}}}
	encoded, err := encodeCommand(original.GetOps())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeCommand(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(original, decoded) {
		t.Fatal("command payload changed")
	}
	legacy, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeCommand(legacy); err == nil || !strings.Contains(err.Error(), "migrate offline") {
		t.Fatalf("unguarded legacy command accepted: %v", err)
	}
	encoded[len(commandMagic)] = 1
	if _, err = decodeCommand(encoded); err == nil || !strings.Contains(err.Error(), "version 1") {
		t.Fatalf("unsupported version accepted: %v", err)
	}
}
