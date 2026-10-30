package spec

import (
	"encoding/json"
	"fmt"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/ui/templates/jsexpr"
)

// backoffPreset defines a named backoff configuration preset.
type backoffPreset struct {
	Label   string  `json:"label"`
	Jitter  string  `json:"jitter"`
	FirstMs int64   `json:"firstMs"`
	MaxMs   int64   `json:"maxMs"`
	Factor  float64 `json:"factor"`
}

var presets = []backoffPreset{
	{Label: "Standard", Jitter: "none", FirstMs: 1000, MaxMs: 5000, Factor: 2.0},
	{Label: "Aggressive", Jitter: "full", FirstMs: 500, MaxMs: 30000, Factor: 3.0},
	{Label: "Gentle", Jitter: "equal", FirstMs: 2000, MaxMs: 10000, Factor: 1.5},
}

// builderXData returns the Alpine x-data expression for the spec
// builder. `seedJSON` must be a valid JSON value (either `null` or an
// object from builderSeed). When non-null the builder pre-populates
// itself from it, turning the create form into an edit form.
//
// Admission is NOT exposed as a field: the CP reconciles desired state and
// upgrades via ApplyTask, which force-replaces at the agent — the only policy
// that converges safely. There is no user knob.
//
// Subprocess supports two modes, reflecting `solti.task.v1.SubprocessTask.mode`:
//
//   - command — a direct binary to exec with args.
//   - script — a body of text (bash/python/node/custom interpreter)
//     passed through the interpreter. The body is transmitted
//     base64-encoded (UTF-8 bytes) on the wire.
func builderXData(agentsEndpoint, seedJSON string) string {
	presetsJSON, _ := json.Marshal(presets)
	if seedJSON == "" {
		seedJSON = "null"
	}
	return fmt.Sprintf("soltiSpecBuilder(%s,%s,%s)", seedJSON, presetsJSON, jsexpr.String(agentsEndpoint))
}

// builderSeed renders a Spec DTO as a JSON literal suitable for
// embedding in Alpine x-data. Returns `null` when no initial value is
// provided (create mode).
func builderSeed(initial *restv1.Spec) string {
	if initial == nil {
		return "null"
	}
	b, err := json.Marshal(initial)
	if err != nil {
		// On a marshalling error fall back to "null": the form will
		// render blank and the user can retype. Swallowing the error
		// here is preferable to an outright render failure.
		return "null"
	}
	return string(b)
}

// builderInitExpr returns the Alpine x-init expression that loads active agents.
func builderInitExpr() string { return "loadAgents()" }

// Conflicts and failures keep the form intact for recovery.
func builderSubmitExpr(endpoint, method string) string {
	return fmt.Sprintf("submit(%s,%s)", jsexpr.String(endpoint), jsexpr.String(method))
}
