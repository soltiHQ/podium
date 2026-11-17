package wire_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/soltiHQ/control-plane/api/gen/solti/agent/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
)

func TestRunnerCapabilitiesSurviveWireRoundTripAndClone(t *testing.T) {
	want := []model.RunnerCapability{{Name: "subprocess", Labels: map[string]string{"zone": "eu"}, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Subprocess"}, {Group: "chain.solti.io", Version: "v1alpha1", Kind: "Chain"}}}}
	agent, _ := model.NewAgent("agent", "Agent", "http://127.0.0.1:1234")
	agent.SetCapabilities(want)
	back, err := wire.AgentFromProto(wire.AgentToProto(agent))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Capabilities(), want) {
		t.Fatalf("capabilities lost: %+v", back.Capabilities())
	}
	clone := back.Capabilities()
	clone[0].Labels["zone"] = "mutated"
	clone[0].Workloads[0].Kind = "Mutated"
	if !reflect.DeepEqual(back.Capabilities(), want) {
		t.Fatal("nested capabilities escaped ownership")
	}
}

func TestCapabilitiesCarryTheSchemaFingerprint(t *testing.T) {
	const fp = "3f1a9c0e5b7d2a64c8e1f09b3d5a7c2e4f6081a3c5e7d9b1f302a4c6e8d0b2f4"
	pb := &agentv1.AgentCapabilities{Runners: []*agentv1.RunnerCapability{{
		Name:   "subprocess",
		Labels: map[string]string{},
		Workloads: []*agentv1.WorkloadType{
			{ApiVersion: "solti.io/v1", Kind: "Subprocess", SchemaFingerprint: fp},
			{ApiVersion: "solti.io/v1", Kind: "Legacy"},
		},
	}}}

	got := wire.CapabilitiesFromProto(pb)
	want := []model.RunnerCapability{{Name: "subprocess", Labels: map[string]string{}, Workloads: []model.GroupVersionKind{
		{Group: "solti.io", Version: "v1", Kind: "Subprocess", SchemaFingerprint: fp},
		{Group: "solti.io", Version: "v1", Kind: "Legacy"},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proto to model lost the fingerprint: %+v", got)
	}
	if back := wire.CapabilitiesToProto(got); !proto.Equal(back, pb) {
		t.Fatalf("model to proto lost the fingerprint: %v", back)
	}

	agent, _ := model.NewAgent("agent", "Agent", "http://127.0.0.1:1234")
	agent.SetCapabilities(got)
	rest := wire.AgentToREST(agent)
	if g := rest.Capabilities[0].Workloads[0].SchemaFingerprint; g != fp {
		t.Fatalf("REST fingerprint = %q, want %q", g, fp)
	}
	if g := rest.Capabilities[0].Workloads[1].SchemaFingerprint; g != "" {
		t.Fatalf("REST fingerprint of a workload without one = %q", g)
	}
	raw, err := json.Marshal(rest.Capabilities[0].Workloads[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "schema_fingerprint") {
		t.Fatalf("an empty fingerprint is serialized: %s", raw)
	}
}
