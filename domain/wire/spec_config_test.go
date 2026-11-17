package wire_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
)

// setBody gives spec a workload carrying body.
func setBody(spec *model.Spec, body map[string]any) {
	spec.SetWorkload(model.Workload{APIVersion: "example.workloads/v1", Kind: "Job", SchemaFingerprint: "f", Body: body})
}

func TestTypedWorkloadBodySurvivesPersistence(t *testing.T) {
	spec, _ := model.NewSpec("spec", "Typed", "slot")
	setBody(spec, map[string]any{"args": []string{"literal spaces", "$HOME"}, "env": map[string]string{"KEY": "VALUE"}, "nested": map[string]any{"values": []int{1, 2}}})
	message, err := wire.SpecToProto(spec)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := wire.SpecFromProto(message)
	if err != nil {
		t.Fatal(err)
	}
	config := restored.Workload().Body
	args := config["args"].([]any)
	if len(args) != 2 || args[0] != "literal spaces" || args[1] != "$HOME" || config["env"].(map[string]any)["KEY"] != "VALUE" {
		t.Fatalf("typed config lost: %+v", config)
	}
}

func TestInvalidWorkloadBodyFailsBeforePersistence(t *testing.T) {
	spec, _ := model.NewSpec("spec", "Invalid", "slot")
	setBody(spec, map[string]any{"unsupported": func() {}})
	message, err := wire.SpecToProto(spec)
	if err == nil || message != nil {
		t.Fatal("invalid config silently accepted")
	}
}

func TestWorkloadBodyRejectsLossyIntegers(t *testing.T) {
	for _, value := range []any{int64(9007199254740993), uint64(18446744073709551615), []uint64{9007199254740993}} {
		spec, _ := model.NewSpec("spec", "Large", "slot")
		setBody(spec, map[string]any{"value": value})
		if msg, err := wire.SpecToProto(spec); err == nil || msg != nil {
			t.Fatalf("lossy integer accepted: %v", value)
		}
	}
	spec, _ := model.NewSpec("spec", "Exact", "slot")
	setBody(spec, map[string]any{"value": int64(9007199254740992), "fraction": 0.125})
	if _, err := wire.SpecToProto(spec); err != nil {
		t.Fatal(err)
	}
}

// A number the body cannot keep is refused with its RFC 6901 path, the first in
// key order, so that a save can say where it is.
func TestWorkloadBodyNamesThePathOfALossyNumber(t *testing.T) {
	for _, tc := range []struct {
		body map[string]any
		path string
	}{
		{map[string]any{"value": int64(9007199254740993)}, "/value"},
		{map[string]any{"a": map[string]any{"b": []any{1, uint64(18446744073709551615)}}}, "/a/b/1"},
		{map[string]any{"z": int64(9007199254740993), "a/b": map[string]any{"~": int64(9007199254740995)}}, "/a~1b/~0"},
	} {
		spec, _ := model.NewSpec("spec", "Large", "slot")
		setBody(spec, tc.body)
		_, err := wire.SpecToProto(spec)
		var nerr *wire.NumberError
		if !errors.As(err, &nerr) || nerr.Path != tc.path || !strings.Contains(err.Error(), tc.path+": ") {
			t.Errorf("%v: %v, want a number error at %s", tc.body, err, tc.path)
		}
	}
}
