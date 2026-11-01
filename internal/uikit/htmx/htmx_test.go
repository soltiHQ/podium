package htmx

import "testing"

// An open agent page polls every minute by default, as the agents list and the
// tasks tab do: no heartbeat refreshes it any more.
func TestAgentDetailPollsEveryMinuteByDefault(t *testing.T) {
	if got := defaultConfig().AgentDetailRefresh; got != Every1m {
		t.Fatalf("agent detail refresh = %q, want %q", got, Every1m)
	}
}
