package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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
