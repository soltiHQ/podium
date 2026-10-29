package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"google.golang.org/protobuf/proto"
)

func TestManifestMapsPoliciesRunnerSelectorAndBinaryScript(t *testing.T) {
	spec, _ := model.NewSpec("A1CaseSensitive", "Demo", "demo-slot")
	spec.SetKindConfig(map[string]any{"mode": map[string]any{"script": map[string]any{"interpreter": "/bin/sh", "body": "ZWNobyBoaQo=", "args": []string{"--literal spaces"}}}, "env": []any{map[string]any{"key": "EMPTY", "value": ""}, map[string]any{"key": "VALUE", "value": "literal $HOME"}}, "cwd": "/tmp", "failOnNonZero": true})
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
			Workload struct {
				Spec struct {
					Env []struct {
						Key   string `json:"key"`
						Value string `json:"value"`
					} `json:"env"`
					Mode struct {
						Script struct {
							Body string   `json:"body"`
							Args []string `json:"args"`
						} `json:"script"`
					} `json:"mode"`
				} `json:"spec"`
			} `json:"workload"`
			RunnerSelector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"runnerSelector"`
		} `json:"spec"`
	}
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Spec.Timeout != 30000 || out.Spec.Restart.Type != "always" || out.Spec.Restart.Interval != 2500 || out.Spec.Workload.Spec.Mode.Script.Body != "ZWNobyBoaQo=" || !reflect.DeepEqual(out.Spec.Workload.Spec.Mode.Script.Args, []string{"--literal spaces"}) || len(out.Spec.Workload.Spec.Env) != 2 || out.Spec.Workload.Spec.Env[0].Key != "EMPTY" || out.Spec.Workload.Spec.Env[0].Value != "" || out.Spec.Workload.Spec.Env[1].Value != "literal $HOME" || out.Spec.RunnerSelector.MatchLabels["zone"] != "eu" {
		t.Fatalf("HTTP encoding lost fields: %s", data)
	}
}

func TestLegacySubprocessOneofNormalizesLosslessly(t *testing.T) {
	for _, variant := range []string{"command", "script"} {
		config := map[string]any{"command": "echo", "args": []string{"one argument", "$HOME"}}
		if variant == "script" {
			config = map[string]any{"interpreter": "sh", "body": "ZWNobyBoaQo=", "args": []string{"-e"}}
		}
		common := map[string]any{"cwd": "/tmp", "failOnNonZero": true, "env": []any{map[string]any{"key": "X", "value": "value"}}}
		legacy, current := map[string]any{}, map[string]any{}
		for k, v := range common {
			legacy[k] = v
			current[k] = v
		}
		legacy[variant] = config
		current["mode"] = map[string]any{variant: config}
		a, err := buildTaskWorkload(enum.TaskKindSubprocess, legacy)
		if err != nil {
			t.Fatal(err)
		}
		b, err := buildTaskWorkload(enum.TaskKindSubprocess, current)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(a, b) {
			t.Fatalf("legacy %s normalization lost data", variant)
		}
	}
}

func TestInvalidWorkloadFieldsAreRejected(t *testing.T) {
	cases := []map[string]any{
		{"mode": map[string]any{"command": map[string]any{"command": "echo", "argz": []string{"lost"}}}},
		{"mode": map[string]any{"command": map[string]any{"command": ""}}},
		{"mode": map[string]any{"script": map[string]any{"interpreter": "sh", "body": "//4="}}},
		{"command": map[string]any{"command": "echo"}, "script": map[string]any{"interpreter": "sh", "body": "ZWNobw=="}},
	}
	for _, cfg := range cases {
		if _, err := buildTaskWorkload(enum.TaskKindSubprocess, cfg); err == nil {
			t.Fatalf("accepted invalid config: %+v", cfg)
		}
	}
}

func TestContainerEntrypointPresenceSurvivesHTTPEncoding(t *testing.T) {
	spec, _ := model.NewSpec("container", "Demo", "container-slot")
	spec.SetKindType(enum.TaskKindContainer)
	for _, entrypoint := range []bool{false, true} {
		config := map[string]any{"image": "example:v1"}
		if entrypoint {
			config["command"] = map[string]any{"items": []string{}}
		}
		spec.SetKindConfig(config)
		manifest, err := SpecToProto(spec)
		if err != nil {
			t.Fatal(err)
		}
		data, err := TaskManifestJSON(manifest)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err = json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		workload := out["spec"].(map[string]any)["workload"].(map[string]any)["spec"].(map[string]any)
		_, exists := workload["command"]
		if exists != entrypoint {
			t.Fatalf("entrypoint presence lost: %s", data)
		}
	}
}

func TestHTTPPreservesNonzeroPolicyAndOrderedDuplicateEnvironment(t *testing.T) {
	for _, fail := range []bool{false, true} {
		spec, _ := model.NewSpec("env-policy", "Demo", "env-policy")
		spec.SetKindConfig(map[string]any{"mode": map[string]any{"command": map[string]any{"command": "echo"}}, "failOnNonZero": fail, "env": []any{map[string]any{"key": "X", "value": "one"}, map[string]any{"key": "X", "value": "two"}}})
		manifest, err := SpecToProto(spec)
		if err != nil {
			t.Fatal(err)
		}
		data, err := TaskManifestJSON(manifest)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err = json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		config := out["spec"].(map[string]any)["workload"].(map[string]any)["spec"].(map[string]any)
		value, present := config["failOnNonZero"]
		if !present || value != fail {
			t.Fatalf("nonzero policy changed: %s", data)
		}
		env, ok := config["env"].([]any)
		if !ok || len(env) != 2 || env[0].(map[string]any)["value"] != "one" || env[1].(map[string]any)["value"] != "two" {
			t.Fatalf("ordered duplicate environment changed: %s", data)
		}
	}
}

func TestManifestRejectsInvalidSlotsIntervalsAndRunnerLabels(t *testing.T) {
	valid := func() *model.Spec {
		s, _ := model.NewSpec("spec", "Demo", "slot")
		s.SetKindConfig(map[string]any{"mode": map[string]any{"command": map[string]any{"command": "echo"}}})
		return s
	}
	for _, slot := range []string{".", "..", "slash/path", "unicode-é", string(make([]byte, 65))} {
		spec, _ := model.NewSpec("spec", "Demo", slot)
		spec.SetKindConfig(valid().KindConfig())
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
	spec.SetRunnerLabels(map[string]string{"example.io/Valid_Key": ""})
	if _, err := SpecToProto(spec); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyContainerCommandPreservesDefaultAndArguments(t *testing.T) {
	for _, items := range [][]string{{}, {"echo", "one argument", "$HOME"}} {
		legacy, err := buildTaskWorkload(enum.TaskKindContainer, map[string]any{"image": "example:v1", "command": items, "args": []string{"literal"}})
		if err != nil {
			t.Fatal(err)
		}
		current := map[string]any{"image": "example:v1", "args": []string{"literal"}}
		if len(items) > 0 {
			current["command"] = map[string]any{"items": items}
		}
		expected, err := buildTaskWorkload(enum.TaskKindContainer, current)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(legacy, expected) {
			t.Fatalf("legacy command changed: %v", items)
		}
	}
}

func TestLegacyInlineScriptRuntimeRequiresExplicitMigration(t *testing.T) {
	for _, runtime := range []map[string]any{{"wellKnown": "SCRIPT_RUNTIME_BASH"}, {"well_known": "SCRIPT_RUNTIME_PYTHON"}, {"custom": map[string]any{"command": "ruby", "flag": "-e"}}} {
		runtime["body"] = "ZWNobw=="
		if _, err := buildTaskWorkload(enum.TaskKindSubprocess, map[string]any{"script": runtime}); err == nil || !strings.Contains(err.Error(), "explicit migration") {
			t.Fatalf("legacy execution semantics silently changed: %v", err)
		}
	}
}
