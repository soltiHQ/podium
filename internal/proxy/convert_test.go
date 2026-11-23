package proxy

import (
	"testing"

	"github.com/soltiHQ/control-plane/domain/model"
)

func TestManifestRejectsInvalidSlotsIntervalsAndRunnerLabels(t *testing.T) {
	valid := func() *model.Spec {
		s, _ := model.NewSpec("spec", "Demo", "slot")
		s.SetWorkload(model.Workload{APIVersion: "example.io/v2", Kind: "Widget", SchemaFingerprint: "abc123", Body: map[string]any{"a": "b"}})
		return s
	}
	for _, slot := range []string{".", "..", "slash/path", "unicode-é", string(make([]byte, 65))} {
		spec, _ := model.NewSpec("spec", "Demo", slot)
		spec.SetWorkload(valid().Workload())
		if _, err := SpecToProto(spec); err == nil {
			t.Fatalf("invalid slot %q accepted", slot)
		}
	}
	for _, label := range []map[string]string{{"bad/prefix/key": "value"}, {"Upper.io/key": "value"}, {"key": "bad value"}, {"-bad": "value"}} {
		spec := valid()
		spec.SetRunnerLabels(label)
		if _, err := SpecToProto(spec); err == nil {
			t.Fatalf("invalid label accepted: %v", label)
		}
	}
	for _, interval := range []int64{-1, 100} {
		spec := valid()
		spec.SetIntervalMs(interval)
		if _, err := SpecToProto(spec); err == nil {
			t.Fatalf("never interval %d accepted", interval)
		}
	}
	spec := valid()
	spec.SetTimeoutMs(0)
	if _, err := SpecToProto(spec); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
	spec = valid()
	spec.SetRunnerLabels(map[string]string{"example.io/Valid_Key": ""})
	if _, err := SpecToProto(spec); err != nil {
		t.Fatal(err)
	}
}
