package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
)

func renderExecution(t *testing.T, task proxyv1.Task) string {
	t.Helper()
	var buf bytes.Buffer
	if err := TaskExecution("agent", task, nil, policy.AgentDetail{}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestReconciliationFailureVisibleWithProcessedGenerationAndNoExecutionError(t *testing.T) {
	html := renderExecution(t, proxyv1.Task{Generation: 4, ObservedGeneration: 4, Status: "pending", Conditions: []proxyv1.TaskCondition{{Type: "Reconciled", Status: "False", ObservedGeneration: 4, Reason: "RunnerNotFound", Message: "No compatible workload runner"}}})
	for _, want := range []string{"Observed execution phase", "Reconciliation blocked", "RunnerNotFound", "No compatible workload runner", "No recorded attempts."} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q in %s", want, html)
		}
	}
	if strings.Contains(html, "Reconciliation accepted at intake") {
		t.Fatal("failed reconciliation appears accepted")
	}
}

func TestStaleAcceptanceDoesNotConfirmCurrentGeneration(t *testing.T) {
	html := renderExecution(t, proxyv1.Task{Generation: 5, Status: "pending", Conditions: []proxyv1.TaskCondition{{Type: "Reconciled", Status: "True", ObservedGeneration: 4}}})
	if strings.Contains(html, "Reconciliation accepted at intake") || !strings.Contains(html, "does not confirm the current desired state") {
		t.Fatal(html)
	}
}

func TestConditionStatesAndUnspecifiedDiagnostics(t *testing.T) {
	task := proxyv1.Task{Generation: 1}
	for _, tc := range []struct{ status, summary string }{{"True", "Reconciliation accepted at intake"}, {"False", "Reconciliation blocked"}, {"Unknown", "Reconciliation pending"}, {"", "Reconciliation status unavailable"}, {"Future", "Reconciliation status unavailable"}} {
		c := proxyv1.TaskCondition{Type: "Reconciled", Status: tc.status, ObservedGeneration: 1}
		if got := conditionSummary(task, c); got != tc.summary {
			t.Fatalf("status %q: %s", tc.status, got)
		}
	}
	if html := renderExecution(t, task); !strings.Contains(html, "agent reported no conditions") {
		t.Fatal(html)
	}
	epoch := int64(0)
	if conditionTime(&epoch) != "1970-01-01 00:00:00 UTC" || conditionTime(nil) != "Not reported" {
		t.Fatal("missing transition timestamp and zero timestamp conflated")
	}
}

func TestAgentConditionDiagnosticsAreEscaped(t *testing.T) {
	html := renderExecution(t, proxyv1.Task{Generation: 1, Conditions: []proxyv1.TaskCondition{{Type: "<script>", Reason: "<script>", Message: "<script>"}}})
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal(html)
	}
}
