package model

import (
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
)

// A later row of an agent shows a reconnect when the agent is active again
// after it was not, or when its uptime went down: it restarted.
func TestAgentReconnectedSince(t *testing.T) {
	active, inactive, disconnected := enum.AgentStatusActive, enum.AgentStatusInactive, enum.AgentStatusDisconnected
	for _, tc := range []struct {
		name      string
		status    enum.AgentStatus // the earlier row
		uptime    int64
		nowStatus enum.AgentStatus // the later row
		nowUptime int64
		want      bool
	}{
		{"active, uptime grew", active, 100, active, 130, false},
		{"active, same uptime", active, 100, active, 100, false},
		{"active, uptime went down", active, 100, active, 5, true},
		{"inactive, now active", inactive, 100, active, 130, true},
		{"disconnected, now active", disconnected, 100, active, 130, true},
		{"active, now inactive", active, 100, inactive, 100, false},
		{"inactive, still inactive", inactive, 100, inactive, 100, false},
		{"inactive, now disconnected", inactive, 100, disconnected, 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag, err := NewAgent("agent-a", "a", "http://agent-a")
			if err != nil {
				t.Fatal(err)
			}
			ag.SetStatus(tc.nowStatus)
			ag.SetUptimeSeconds(tc.nowUptime)
			if got := ag.ReconnectedSince(tc.status, tc.uptime); got != tc.want {
				t.Fatalf("ReconnectedSince(%s, %d) of a row %s with uptime %d = %v, want %v",
					tc.status, tc.uptime, tc.nowStatus, tc.nowUptime, got, tc.want)
			}
		})
	}
}
