package wire_test

import (
	"reflect"
	"testing"

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
