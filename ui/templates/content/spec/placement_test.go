package spec

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
)

func renderPlacement(t *testing.T, plan restv1.SpecPlacement) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Placement(plan).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestPlacementPreservesRunnerOrderAndExplainsBatchBlock(t *testing.T) {
	html := renderPlacement(t, restv1.SpecPlacement{SpecID: "s", SpecVersion: 7, Generation: 3, CheckedAt: "2026-10-02T03:00:00Z", BlockingReason: "unsupported_targets", Targets: []restv1.PlacementTarget{{AgentID: "a", Exists: true, InventoryKnown: true, Eligible: true, EligibilityReason: "compatible_runner", SelectedRunner: "z-first", Runners: []restv1.PlacementRunner{{Name: "z-first", Eligible: true, Reason: "compatible"}, {Name: "a-later", Reason: "runner_labels_mismatch"}}}}})
	for _, want := range []string{"saved v7 · gen 3", "Deployment blocked", "blocked for every target", "First compatible runner", "2026-10-02 03:00:00 UTC", "Unsaved edits are excluded", "not active health checks"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q in %s", want, html)
		}
	}
	if strings.Index(html, "1. z-first") > strings.Index(html, "2. a-later") || !strings.Contains(html, "does not match all required runner labels") {
		t.Fatal("runner registration order or exclusion lost")
	}
}

func TestPlacementMakesUnknownInventoryAndEmptyTargetsExplicit(t *testing.T) {
	html := renderPlacement(t, restv1.SpecPlacement{DeployAllowed: true, Targets: []restv1.PlacementTarget{{AgentID: "legacy", Exists: true, Eligible: true, EligibilityReason: "inventory_unknown"}}})
	if !strings.Contains(html, "Runner inventory unknown") || !strings.Contains(html, "runner compatibility is unknown") || strings.Contains(html, "Routing compatible") {
		t.Fatal(html)
	}
	html = renderPlacement(t, restv1.SpecPlacement{DeployAllowed: true, NoTargets: true})
	if !strings.Contains(html, "Deploy removes existing rollout tasks") {
		t.Fatal(html)
	}
}

func TestPlacementEscapesDiscoveryLabelsAndNames(t *testing.T) {
	html := renderPlacement(t, restv1.SpecPlacement{Targets: []restv1.PlacementTarget{{AgentName: "<script>", Exists: true, Runners: []restv1.PlacementRunner{{Name: "<script>", Labels: map[string]string{"key": "<script>"}}}}}})
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal(html)
	}
	if got := placementLabels(map[string]string{"z": "2", "a": "1"}); got != "a=1, z=2" {
		t.Fatal(got)
	}
}

func TestSpecJSONPreviewUsesCurrentTaskManifestLabel(t *testing.T) {
	var buf bytes.Buffer
	dto := restv1.RolloutSpec{Spec: restv1.Spec{CreateSpec: json.RawMessage(`{"apiVersion":"solti.io/v1","kind":"Task","metadata":{"name":"task"}}`)}}
	if err := Detail(dto, policy.SpecDetail{}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if !strings.Contains(html, "Agent Task Manifest") || strings.Contains(html, "Agent CreateSpec") {
		t.Fatal("preview uses obsolete API label")
	}
}
