package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
)

// TaskNameForSpec gives a control-plane spec one stable DNS-1123 resource name.
// Spec IDs are case-sensitive KSUIDs; hashing preserves their distinct identity.
func TaskNameForSpec(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("podium-%x", sum)
}

// SpecToProto converts desired control-plane state to the current Task manifest.
// KindConfig uses strict protobuf JSON for the selected built-in workload spec.
func SpecToProto(ts *model.Spec) (*taskv1.TaskManifest, error) {
	if ts == nil {
		return nil, fmt.Errorf("proxy: nil spec")
	}
	if !slotPattern.MatchString(ts.Slot()) || len(ts.Slot()) > 64 || ts.Slot() == "." || ts.Slot() == ".." {
		return nil, fmt.Errorf("proxy: slot must contain 1 to 64 ASCII letters, digits, '.', '_' or '-'")
	}
	if ts.IntervalMs() < 0 || (ts.RestartType() != enum.RestartAlways && ts.IntervalMs() != 0) {
		return nil, fmt.Errorf("proxy: restart interval must be non-negative and is valid only for always")
	}
	if len(ts.ID())+len("podium.solti.io/spec-id") > 256*1024 {
		return nil, fmt.Errorf("proxy: spec identity exceeds annotation byte limit")
	}
	for key, value := range ts.RunnerLabels() {
		if !validLabelKey(key) || (value != "" && (len(value) > 63 || !labelPartPattern.MatchString(value))) {
			return nil, fmt.Errorf("proxy: invalid runner label %q", key)
		}
	}
	workload, err := buildTaskWorkload(ts.KindType(), ts.KindConfig())
	if err != nil {
		return nil, err
	}
	b := ts.Backoff()
	if ts.TimeoutMs() <= 0 || b.FirstMs <= 0 || b.MaxMs < b.FirstMs || math.IsNaN(b.Factor) || math.IsInf(b.Factor, 0) || b.Factor < 1 {
		return nil, fmt.Errorf("proxy: invalid timeout or backoff policy")
	}
	restart := restartStrategyToProto(ts.RestartType())
	jitter := jitterStrategyToProto(b.Jitter)
	if restart == taskv1.RestartPolicy_RESTART_POLICY_UNSPECIFIED || jitter == taskv1.JitterPolicy_JITTER_POLICY_UNSPECIFIED {
		return nil, fmt.Errorf("proxy: unsupported restart or jitter policy")
	}
	spec := &taskv1.TaskSpec{
		Slot: ts.Slot(), Workload: workload, TimeoutMs: uint64(ts.TimeoutMs()), Restart: restart,
		Backoff:   &taskv1.BackoffPolicy{Jitter: jitter, FirstMs: uint64(b.FirstMs), MaxMs: uint64(b.MaxMs), Factor: b.Factor},
		Admission: taskv1.AdmissionPolicy_ADMISSION_POLICY_REPLACE,
	}
	if ts.RestartType() == enum.RestartAlways && ts.IntervalMs() > 0 {
		iv := uint64(ts.IntervalMs())
		spec.RestartIntervalMs = &iv
	}
	if labels := ts.RunnerLabels(); len(labels) > 0 {
		spec.RunnerSelector = &taskv1.LabelSelector{MatchLabels: labels}
	}
	return &taskv1.TaskManifest{
		ApiVersion: "solti.io/v1", Kind: "Task",
		Metadata: &taskv1.TaskManifestMeta{Name: TaskNameForSpec(ts.ID()), Annotations: map[string]string{"podium.solti.io/spec-id": ts.ID()}},
		Spec:     spec,
	}, nil
}

func buildTaskWorkload(kt enum.TaskKindType, cfg map[string]any) (*taskv1.TaskWorkload, error) {
	field, kind := "", ""
	switch kt {
	case enum.TaskKindSubprocess:
		field, kind = "subprocess", "Subprocess"
	case enum.TaskKindContainer:
		field, kind = "container", "Container"
	case enum.TaskKindWasm:
		field, kind = "wasm", "Wasm"
	default:
		return nil, fmt.Errorf("proxy: unknown task kind %q", kt)
	}
	// Earlier Podium specs used an inline command/script oneof. Normalize only
	// that exact shape; strict decoding still rejects unknown or mixed fields.
	if kt == enum.TaskKindSubprocess {
		if _, current := cfg["mode"]; !current {
			_, command := cfg["command"]
			_, script := cfg["script"]
			if command != script {
				normalized := make(map[string]any, len(cfg))
				for key, value := range cfg {
					normalized[key] = value
				}
				variant := "command"
				if script {
					variant = "script"
				}
				normalized["mode"] = map[string]any{variant: normalized[variant]}
				delete(normalized, variant)
				cfg = normalized
			}
		}
	}
	if kt == enum.TaskKindSubprocess {
		if mode, ok := cfg["mode"].(map[string]any); ok {
			if script, ok := mode["script"].(map[string]any); ok {
				if _, old := script["wellKnown"]; old {
					return nil, fmt.Errorf("proxy: legacy wellKnown script runtime requires explicit migration to an interpreter; inline-script and file-script arguments differ")
				}
				if _, old := script["well_known"]; old {
					return nil, fmt.Errorf("proxy: legacy well_known script runtime requires explicit migration to an interpreter; inline-script and file-script arguments differ")
				}
				if _, old := script["custom"]; old {
					return nil, fmt.Errorf("proxy: legacy custom script runtime requires explicit migration; its inline flag cannot be dropped when choosing a file interpreter")
				}
			}
		}
	}
	if kt == enum.TaskKindContainer {
		if command, exists := cfg["command"]; exists {
			switch items := command.(type) {
			case []string:
				normalized := make(map[string]any, len(cfg))
				for key, value := range cfg {
					normalized[key] = value
				}
				if len(items) == 0 {
					delete(normalized, "command")
				} else {
					normalized["command"] = map[string]any{"items": items}
				}
				cfg = normalized
			case []any:
				normalized := make(map[string]any, len(cfg))
				for key, value := range cfg {
					normalized[key] = value
				}
				if len(items) == 0 {
					delete(normalized, "command")
				} else {
					normalized["command"] = map[string]any{"items": items}
				}
				cfg = normalized
			}
		}
	}
	data, err := json.Marshal(map[string]any{"apiVersion": "solti.io/v1", "kind": kind, field: cfg})
	if err != nil {
		return nil, fmt.Errorf("proxy: marshal workload: %w", err)
	}
	var out taskv1.TaskWorkload
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("proxy: invalid %s config: %w", kt, err)
	}
	switch kt {
	case enum.TaskKindSubprocess:
		sub := out.GetSubprocess()
		if sub == nil || sub.GetMode() == nil || sub.GetMode().GetMode() == nil {
			return nil, fmt.Errorf("proxy: subprocess requires mode.command or mode.script")
		}
		if cmd := sub.GetMode().GetCommand(); cmd != nil && strings.TrimSpace(cmd.GetCommand()) == "" {
			return nil, fmt.Errorf("proxy: subprocess command must not be empty")
		}
		if script := sub.GetMode().GetScript(); script != nil {
			if len(script.GetBody()) > ((2*1024*1024+2)/3)*4 {
				return nil, fmt.Errorf("proxy: script body exceeds 2 MiB decoded limit")
			}
			body, err := base64.StdEncoding.DecodeString(script.GetBody())
			if strings.TrimSpace(script.GetInterpreter()) == "" || err != nil || len(body) == 0 || len(body) > 2*1024*1024 || !utf8.Valid(body) {
				return nil, fmt.Errorf("proxy: script requires an interpreter and non-empty UTF-8 base64 body up to 2 MiB")
			}
		}
	case enum.TaskKindContainer:
		if strings.TrimSpace(out.GetContainer().GetImage()) == "" {
			return nil, fmt.Errorf("proxy: container image must not be empty")
		}
	case enum.TaskKindWasm:
		if strings.TrimSpace(out.GetWasm().GetModule()) == "" {
			return nil, fmt.Errorf("proxy: wasm module must not be empty")
		}
	}
	return &out, nil
}

// CreateSpecWirePreview returns the exact HTTP TaskManifest JSON sent to agents.
func CreateSpecWirePreview(manifest *taskv1.TaskManifest) (json.RawMessage, error) {
	if manifest == nil {
		return nil, nil
	}
	return TaskManifestJSON(manifest)
}

// TaskManifestJSON encodes SDK resource JSON. Task API HTTP does not use protojson.
func TaskManifestJSON(manifest *taskv1.TaskManifest) ([]byte, error) {
	if manifest == nil || manifest.GetMetadata() == nil || manifest.GetSpec() == nil || manifest.GetSpec().GetWorkload() == nil {
		return nil, fmt.Errorf("proxy: incomplete task manifest")
	}
	spec := manifest.GetSpec()
	w := spec.GetWorkload()
	data, err := protojson.Marshal(w)
	if err != nil {
		return nil, err
	}
	var typed map[string]json.RawMessage
	if err = json.Unmarshal(data, &typed); err != nil {
		return nil, err
	}
	var config map[string]any
	field := ""
	switch w.GetSpec().(type) {
	case *taskv1.TaskWorkload_Subprocess:
		field = "subprocess"
	case *taskv1.TaskWorkload_Container:
		field = "container"
	case *taskv1.TaskWorkload_Wasm:
		field = "wasm"
	default:
		return nil, fmt.Errorf("proxy: unsupported HTTP workload")
	}
	if err = json.Unmarshal(typed[field], &config); err != nil {
		return nil, err
	}
	if field == "subprocess" {
		// The SDK's HTTP default is true; a protobuf false must stay explicit.
		config["failOnNonZero"] = w.GetSubprocess().GetFailOnNonZero()
	}
	if command, ok := config["command"].(map[string]any); ok && field == "container" {
		items := command["items"]
		if items == nil {
			items = []string{}
		}
		config["command"] = items
	}
	restart := map[string]any{"type": restartPolicyString(spec.GetRestart())}
	if spec.RestartIntervalMs != nil {
		restart["intervalMs"] = *spec.RestartIntervalMs
	}
	backoff := spec.GetBackoff()
	if backoff == nil {
		return nil, fmt.Errorf("proxy: missing backoff")
	}
	desired := map[string]any{
		"slot": spec.GetSlot(), "workload": map[string]any{"apiVersion": w.GetApiVersion(), "kind": w.GetKind(), "spec": config},
		"timeout": spec.GetTimeoutMs(), "restart": restart,
		"backoff":   map[string]any{"jitter": jitterPolicyString(backoff.GetJitter()), "firstMs": backoff.GetFirstMs(), "maxMs": backoff.GetMaxMs(), "factor": backoff.GetFactor()},
		"admission": admissionPolicyString(spec.GetAdmission()),
	}
	if spec.MaxRetries != nil {
		desired["maxRetries"] = *spec.MaxRetries
	}
	if selector := spec.GetRunnerSelector(); selector != nil {
		if len(selector.GetMatchExpressions()) > 0 {
			return nil, fmt.Errorf("proxy: unsupported selector expression")
		}
		desired["runnerSelector"] = map[string]any{"matchLabels": selector.GetMatchLabels()}
	}
	metadata := map[string]any{"name": manifest.GetMetadata().GetName()}
	if labels := manifest.GetMetadata().GetLabels(); len(labels) > 0 {
		metadata["labels"] = labels
	}
	if annotations := manifest.GetMetadata().GetAnnotations(); len(annotations) > 0 {
		metadata["annotations"] = annotations
	}
	return json.Marshal(map[string]any{"apiVersion": manifest.GetApiVersion(), "kind": manifest.GetKind(), "metadata": metadata, "spec": desired})
}

func restartStrategyToProto(rt enum.RestartType) taskv1.RestartPolicy {
	switch rt {
	case enum.RestartNever:
		return taskv1.RestartPolicy_RESTART_POLICY_NEVER
	case enum.RestartOnFailure:
		return taskv1.RestartPolicy_RESTART_POLICY_ON_FAILURE
	case enum.RestartAlways:
		return taskv1.RestartPolicy_RESTART_POLICY_ALWAYS
	default:
		return taskv1.RestartPolicy_RESTART_POLICY_UNSPECIFIED
	}
}

func jitterStrategyToProto(j enum.JitterStrategy) taskv1.JitterPolicy {
	switch j {
	case enum.JitterNone:
		return taskv1.JitterPolicy_JITTER_POLICY_NONE
	case enum.JitterFull:
		return taskv1.JitterPolicy_JITTER_POLICY_FULL
	case enum.JitterEqual:
		return taskv1.JitterPolicy_JITTER_POLICY_EQUAL
	case enum.JitterDecorrelated:
		return taskv1.JitterPolicy_JITTER_POLICY_DECORRELATED
	default:
		return taskv1.JitterPolicy_JITTER_POLICY_UNSPECIFIED
	}
}

func restartPolicyString(p taskv1.RestartPolicy) string {
	return map[taskv1.RestartPolicy]string{taskv1.RestartPolicy_RESTART_POLICY_NEVER: "never", taskv1.RestartPolicy_RESTART_POLICY_ON_FAILURE: "onFailure", taskv1.RestartPolicy_RESTART_POLICY_ALWAYS: "always"}[p]
}

func jitterPolicyString(p taskv1.JitterPolicy) string {
	return map[taskv1.JitterPolicy]string{taskv1.JitterPolicy_JITTER_POLICY_NONE: "none", taskv1.JitterPolicy_JITTER_POLICY_FULL: "full", taskv1.JitterPolicy_JITTER_POLICY_EQUAL: "equal", taskv1.JitterPolicy_JITTER_POLICY_DECORRELATED: "decorrelated"}[p]
}

func admissionPolicyString(p taskv1.AdmissionPolicy) string {
	return map[taskv1.AdmissionPolicy]string{taskv1.AdmissionPolicy_ADMISSION_POLICY_REPLACE: "replace", taskv1.AdmissionPolicy_ADMISSION_POLICY_DROP_IF_RUNNING: "dropIfRunning", taskv1.AdmissionPolicy_ADMISSION_POLICY_QUEUE: "queue"}[p]
}

var (
	slotPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	labelPartPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$`)
	dnsSegmentPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

func validLabelKey(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) > 2 {
		return false
	}
	name := parts[len(parts)-1]
	if len(name) == 0 || len(name) > 63 || !labelPartPattern.MatchString(name) {
		return false
	}
	if len(parts) == 2 {
		if len(parts[0]) == 0 || len(parts[0]) > 253 {
			return false
		}
		for _, segment := range strings.Split(parts[0], ".") {
			if !dnsSegmentPattern.MatchString(segment) {
				return false
			}
		}
	}
	return true
}
