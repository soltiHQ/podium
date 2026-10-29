package model

import "testing"

func TestSpecNestedConfigDoesNotAlias(t *testing.T) {
	s, _ := NewSpec("id", "name", "slot")
	input := map[string]any{"command": map[string]any{"args": []any{"safe"}}}
	s.SetKindConfig(input)
	input["command"].(map[string]any)["args"].([]any)[0] = "input mutation"
	clone := s.Clone()
	clone.kindConfig["command"].(map[string]any)["args"].([]any)[0] = "clone mutation"
	output := s.KindConfig()
	output["command"].(map[string]any)["args"].([]any)[0] = "getter mutation"
	got := s.KindConfig()["command"].(map[string]any)["args"].([]any)[0]
	if got != "safe" {
		t.Fatalf("config alias leaked: %v", got)
	}
}
