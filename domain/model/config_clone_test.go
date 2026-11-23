package model

import "testing"

func TestSpecWorkloadBodyDoesNotAlias(t *testing.T) {
	s, _ := NewSpec("id", "name", "slot")
	input := map[string]any{"command": map[string]any{"args": []any{"safe"}}}
	s.SetWorkload(Workload{APIVersion: "example.workloads/v1", Kind: "Job", SchemaFingerprint: "f", Body: input})
	input["command"].(map[string]any)["args"].([]any)[0] = "input mutation"
	clone := s.Clone()
	clone.workload.Body["command"].(map[string]any)["args"].([]any)[0] = "clone mutation"
	output := s.Workload()
	output.Body["command"].(map[string]any)["args"].([]any)[0] = "getter mutation"
	got := s.Workload().Body["command"].(map[string]any)["args"].([]any)[0]
	if got != "safe" {
		t.Fatalf("workload body alias leaked: %v", got)
	}
}
