package spec

import (
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/internal/uikit/policy"
)

func TestPlacementPanelFollowsDedicatedPolicy(t *testing.T) {
	for _, allow := range []bool{false, true} {
		panels := detailPanels("saved", policy.SpecDetail{CanViewPlacement: allow})
		found := false
		for _, p := range panels {
			if p.ID == "spec-placement" {
				found = true
				if !strings.Contains(p.Trigger, "spec_update from:body") || !strings.Contains(p.Trigger, "agent_update from:body") {
					t.Fatal("placement refresh missing")
				}
				if p.URL != "/api/v1/specs/saved/placement" {
					t.Fatal(p.URL)
				}
			}
		}
		if found != allow {
			t.Fatalf("placement panel visible %v with permission %v", found, allow)
		}
	}
}
