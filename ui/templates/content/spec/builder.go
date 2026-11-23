package spec

import (
	"encoding/json"
	"fmt"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
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
func builderXData(agentsEndpoint, typesEndpoint, seedJSON string) string {
	presetsJSON, _ := json.Marshal(presets)
	if seedJSON == "" {
		seedJSON = "null"
	}
	return fmt.Sprintf("soltiSpecBuilder(%s,%s,%s,%s)", seedJSON, presetsJSON, jsexpr.String(agentsEndpoint), jsexpr.String(typesEndpoint))
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

// builderInitExpr returns the Alpine x-init expression that loads the workload
// type catalog and the active agents.
func builderInitExpr() string { return "start()" }

// shortFingerprint is the first 12 characters of a schema fingerprint: enough
// to tell fingerprints apart, with the full value kept in a title attribute.
func shortFingerprint(fp string) string {
	if len(fp) <= 12 {
		return fp
	}
	return fp[:12]
}

// Conflicts and failures keep the form intact for recovery.
func builderSubmitExpr(endpoint, method string) string {
	return fmt.Sprintf("submit(%s,%s)", jsexpr.String(endpoint), jsexpr.String(method))
}

// workloadType is "apiVersion/kind" of a spec's workload, empty for none.
func workloadType(w *restv1.Workload) string {
	if w == nil || w.Kind == "" {
		return ""
	}
	return w.APIVersion + "/" + w.Kind
}

// workloadFingerprint is the schema fingerprint of a spec's workload.
func workloadFingerprint(w *restv1.Workload) string {
	if w == nil {
		return ""
	}
	return w.SchemaFingerprint
}

// restWorkload converts a stored workload to its REST shape, for display.
func restWorkload(w model.Workload) *restv1.Workload {
	return &restv1.Workload{APIVersion: w.APIVersion, Kind: w.Kind, SchemaFingerprint: w.SchemaFingerprint}
}
