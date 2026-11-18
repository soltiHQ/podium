package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
)

// workloadBody is a body with every JSON shape: nested objects, arrays, strings
// that look like numbers, booleans, null and an integer larger than 2^53.
func workloadBody() map[string]any {
	return map[string]any{
		"image":    "busybox:1",
		"command":  []any{"echo", "one argument", "$HOME"},
		"env":      []any{map[string]any{"key": "EMPTY", "value": ""}, map[string]any{"key": "X", "value": "1"}},
		"replicas": json.Number("9007199254740993"),
		"nested":   map[string]any{"on": true, "off": false, "none": nil, "ratio": 0.5},
	}
}

func workloadSpec(t *testing.T) *model.Spec {
	t.Helper()
	spec, err := model.NewSpec("A1CaseSensitive", "Demo", "demo-slot")
	if err != nil {
		t.Fatal(err)
	}
	spec.SetWorkload(model.Workload{APIVersion: "example.io/v2", Kind: "Widget", SchemaFingerprint: "abc123", Body: workloadBody()})
	return spec
}

func TestSpecToProtoSendsTheBodyAsAnExtension(t *testing.T) {
	spec := workloadSpec(t)
	spec.SetRunnerLabels(map[string]string{"zone": "eu"})
	spec.SetRestartType(enum.RestartAlways)
	spec.SetIntervalMs(2500)
	manifest, err := SpecToProto(spec)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.GetMetadata().GetName() != TaskNameForSpec(spec.ID()) || manifest.GetMetadata().GetAnnotations()["podium.solti.io/spec-id"] != spec.ID() {
		t.Fatal("stable identity missing")
	}
	if TaskNameForSpec("a1CaseSensitive") == manifest.GetMetadata().GetName() {
		t.Fatal("case-sensitive IDs collided")
	}
	w := manifest.GetSpec().GetWorkload()
	if w.GetApiVersion() != "example.io/v2" || w.GetKind() != "Widget" {
		t.Fatalf("workload type = %s %s", w.GetApiVersion(), w.GetKind())
	}
	ext := w.GetExtension()
	if ext == nil {
		t.Fatalf("workload is not an extension: %v", w)
	}
	var got map[string]any
	if err = json.Unmarshal(ext.GetSpec().GetRaw(), &got); err != nil {
		t.Fatalf("Raw is not JSON: %v", err)
	}
	var want map[string]any
	exact, _ := json.Marshal(workloadBody())
	if err = json.Unmarshal(exact, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Raw = %s, want the stored body", ext.GetSpec().GetRaw())
	}
	if !strings.Contains(string(ext.GetSpec().GetRaw()), "9007199254740993") {
		t.Fatalf("Raw lost the integer's digits: %s", ext.GetSpec().GetRaw())
	}
	if manifest.GetSpec().GetRunnerSelector().GetMatchLabels()["zone"] != "eu" || manifest.GetSpec().GetRestartIntervalMs() != 2500 {
		t.Fatalf("policies lost: %v", manifest.GetSpec())
	}
}

func TestTaskManifestJSONSendsTheBodyAsSpec(t *testing.T) {
	spec := workloadSpec(t)
	spec.SetRunnerLabels(map[string]string{"zone": "eu"})
	spec.SetRestartType(enum.RestartAlways)
	spec.SetIntervalMs(2500)
	manifest, err := SpecToProto(spec)
	if err != nil {
		t.Fatal(err)
	}
	data, err := TaskManifestJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Spec struct {
			Timeout uint64 `json:"timeout"`
			Restart struct {
				Type     string `json:"type"`
				Interval int    `json:"intervalMs"`
			} `json:"restart"`
			Workload map[string]json.RawMessage `json:"workload"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"runnerSelector"`
		} `json:"spec"`
	}
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Spec.Workload) != 3 {
		t.Fatalf("workload has keys beyond apiVersion, kind and spec: %s", data)
	}
	var apiVersion, kind string
	if json.Unmarshal(out.Spec.Workload["apiVersion"], &apiVersion) != nil || json.Unmarshal(out.Spec.Workload["kind"], &kind) != nil || apiVersion != "example.io/v2" || kind != "Widget" {
		t.Fatalf("workload type lost: %s", data)
	}
	var got, want map[string]any
	if err = json.Unmarshal(out.Spec.Workload["spec"], &got); err != nil {
		t.Fatal(err)
	}
	exact, _ := json.Marshal(workloadBody())
	if err = json.Unmarshal(exact, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workload spec = %s, want the stored body", out.Spec.Workload["spec"])
	}
	if out.Spec.Timeout != 30000 || out.Spec.Restart.Type != "always" || out.Spec.Restart.Interval != 2500 || out.Spec.Selector.MatchLabels["zone"] != "eu" {
		t.Fatalf("HTTP encoding lost policies: %s", data)
	}
}

func TestSpecToProtoRefusesASpecWithoutAWorkload(t *testing.T) {
	spec, _ := model.NewSpec("spec", "Demo", "slot")
	if _, err := SpecToProto(spec); err == nil || !strings.Contains(err.Error(), "spec has no workload") {
		t.Fatalf("a spec without a workload: %v", err)
	}
	spec.SetWorkload(model.Workload{APIVersion: "example.io/v2", Kind: "Widget", SchemaFingerprint: "abc123"})
	manifest, err := SpecToProto(spec)
	if err != nil {
		t.Fatal(err)
	}
	if raw := manifest.GetSpec().GetWorkload().GetExtension().GetSpec().GetRaw(); string(raw) != "{}" {
		t.Fatalf("an empty body is sent as %q, want {}", raw)
	}
}

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
