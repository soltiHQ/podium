package wire_test

import (
	"testing"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
)

func TestTypedSpecConfigSurvivesPersistence(t *testing.T) {
	spec, _ := model.NewSpec("spec", "Typed", "slot")
	spec.SetKindConfig(map[string]any{"args": []string{"literal spaces", "$HOME"}, "env": map[string]string{"KEY": "VALUE"}, "nested": map[string]any{"values": []int{1, 2}}})
	message, err := wire.SpecToProto(spec)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := wire.SpecFromProto(message)
	if err != nil {
		t.Fatal(err)
	}
	config := restored.KindConfig()
	args := config["args"].([]any)
	if len(args) != 2 || args[0] != "literal spaces" || args[1] != "$HOME" || config["env"].(map[string]any)["KEY"] != "VALUE" {
		t.Fatalf("typed config lost: %+v", config)
	}
}

func TestInvalidSpecConfigFailsBeforePersistence(t *testing.T) {
	spec, _ := model.NewSpec("spec", "Invalid", "slot")
	spec.SetKindConfig(map[string]any{"unsupported": func() {}})
	message, err := wire.SpecToProto(spec)
	if err == nil || message != nil {
		t.Fatal("invalid config silently accepted")
	}
}

func TestSpecConfigRejectsLossyIntegers(t *testing.T) {
	for _, value := range []any{int64(9007199254740993), uint64(18446744073709551615), []uint64{9007199254740993}} {
		spec, _ := model.NewSpec("spec", "Large", "slot")
		spec.SetKindConfig(map[string]any{"value": value})
		if msg, err := wire.SpecToProto(spec); err == nil || msg != nil {
			t.Fatalf("lossy integer accepted: %v", value)
		}
	}
	spec, _ := model.NewSpec("spec", "Exact", "slot")
	spec.SetKindConfig(map[string]any{"value": int64(9007199254740992), "fraction": 0.125})
	if _, err := wire.SpecToProto(spec); err != nil {
		t.Fatal(err)
	}
}
