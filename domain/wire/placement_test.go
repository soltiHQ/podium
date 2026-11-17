package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/soltiHQ/control-plane/domain/model"
)

func TestPlacementRESTPreservesRevisionOrderAndMissingAgent(t *testing.T) {
	now := time.Unix(1700000000, 123).UTC()
	plan := &model.PlacementPlan{SpecID: "spec", SpecVersion: 7, Generation: 3, CheckedAt: now, Targets: []model.PlacementTarget{{AgentID: "missing", Status: "unknown", HeartbeatFreshness: "unknown", MatchedBy: []string{"explicit"}, EligibilityReason: "missing_agent"}, {AgentID: "known", LastSeenAt: now, RunnerNames: []string{"z-first", "a-second"}, SelectedRunner: "z-first", Eligible: true, Runners: []model.PlacementRunner{{Name: "z-first", Labels: map[string]string{"zone": "a"}}}}}}
	dto := PlacementToREST(plan)
	if dto.SpecVersion != 7 || dto.Generation != 3 || dto.CheckedAt != now.Format(time.RFC3339Nano) || dto.Targets[1].SelectedRunner != "z-first" {
		t.Fatalf("plan wire values lost: %+v", dto)
	}
	dto.Targets[1].Runners[0].Labels["zone"] = "mutated"
	if plan.Targets[1].Runners[0].Labels["zone"] != "a" {
		t.Fatal("REST output aliases plan routing labels")
	}
	payload, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "0001-") || !strings.Contains(string(payload), `"runner_names":[]`) || !strings.Contains(string(payload), `"runners":[]`) {
		t.Fatalf("missing inventory wire arrays/times: %s", payload)
	}
}

func TestPlacementRESTBoundsTheRunnerDetailAtARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 700) // 1400 bytes, two per rune
	plan := &model.PlacementPlan{Targets: []model.PlacementTarget{{AgentID: "a", Runners: []model.PlacementRunner{
		{Name: "short", Reason: "schema_incompatible", Detail: "/image: wrong type"},
		{Name: "long", Reason: "schema_incompatible", Detail: long},
		{Name: "none", Reason: "compatible"},
	}}}}
	got := PlacementToREST(plan).Targets[0].Runners
	if got[0].Detail != "/image: wrong type" || got[2].Detail != "" {
		t.Fatalf("details = %q, %q", got[0].Detail, got[2].Detail)
	}
	if len(got[1].Detail) > 1024 || !utf8.ValidString(got[1].Detail) || !strings.HasPrefix(long, got[1].Detail) || len(got[1].Detail) < 1022 {
		t.Fatalf("the long detail is %d bytes, valid %v", len(got[1].Detail), utf8.ValidString(got[1].Detail))
	}
	data, err := json.Marshal(got[2])
	if err != nil || strings.Contains(string(data), "detail") {
		t.Fatalf("an empty detail is sent: %s", data)
	}
}
