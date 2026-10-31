package model

import (
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
)

// A later row of an agent shows a visible change when its status, name,
// endpoint, labels or capabilities differ from the earlier row. What a sync
// moves each time, the time of the sync, the uptime and the heartbeat interval,
// is no change, and neither are the fields no operator watches a sync change.
func TestAgentChangedVisiblySince(t *testing.T) {
	runner := func(name, zone string, kinds ...string) RunnerCapability {
		r := RunnerCapability{Name: name, Labels: map[string]string{"zone": zone}}
		for _, kind := range kinds {
			r.Workloads = append(r.Workloads, GroupVersionKind{Group: "solti.io", Version: "v1", Kind: kind})
		}
		return r
	}
	base := func() *Agent {
		a, err := NewAgent("agent-a", "Agent A", "10.0.0.5:50051")
		if err != nil {
			t.Fatal(err)
		}
		a.SetCapabilities([]RunnerCapability{runner("subprocess", "eu", "Subprocess"), runner("container", "eu", "Container")})
		a.LabelAdd("env", "prod")
		return a
	}
	for _, tc := range []struct {
		name   string
		change func(*Agent)
		want   bool
	}{
		{"nothing", func(*Agent) {}, false},
		{"last seen", func(a *Agent) { a.SetLastSeenAt(a.LastSeenAt().Add(time.Minute)) }, false},
		{"updated at", func(a *Agent) { a.SetUpdatedAt(a.UpdatedAt().Add(time.Minute)) }, false},
		{"uptime", func(a *Agent) { a.SetUptimeSeconds(a.UptimeSeconds() + 30) }, false},
		{"heartbeat interval", func(a *Agent) { a.SetHeartbeatInterval(time.Minute) }, false},
		{"stale at", func(a *Agent) { a.SetStaleAt(time.Now().Add(time.Hour)) }, false},
		{"status", func(a *Agent) { a.SetStatus(enum.AgentStatusInactive) }, true},
		{"name", func(a *Agent) { a.name = "Renamed" }, true},
		{"endpoint", func(a *Agent) { a.endpoint = "10.0.0.6:50051" }, true},
		{"label added", func(a *Agent) { a.LabelAdd("zone", "eu") }, true},
		{"label changed", func(a *Agent) { a.LabelAdd("env", "dev") }, true},
		{"label deleted", func(a *Agent) { a.LabelDelete("env") }, true},
		{"runner added", func(a *Agent) { a.SetCapabilities(append(a.Capabilities(), runner("wasm", "eu", "Wasm"))) }, true},
		{"runner removed", func(a *Agent) { a.SetCapabilities(a.Capabilities()[:1]) }, true},
		{"runners reordered", func(a *Agent) {
			c := a.Capabilities()
			c[0], c[1] = c[1], c[0]
			a.SetCapabilities(c)
		}, true},
		{"runner renamed", func(a *Agent) {
			c := a.Capabilities()
			c[0].Name = "process"
			a.SetCapabilities(c)
		}, true},
		{"runner label", func(a *Agent) {
			c := a.Capabilities()
			c[0].Labels["zone"] = "us"
			a.SetCapabilities(c)
		}, true},
		{"runner label added", func(a *Agent) {
			c := a.Capabilities()
			c[0].Labels["pool"] = "general"
			a.SetCapabilities(c)
		}, true},
		{"runner workload", func(a *Agent) {
			c := a.Capabilities()
			c[0].Workloads[0].Kind = "Wasm"
			a.SetCapabilities(c)
		}, true},
		{"runner workload added", func(a *Agent) {
			c := a.Capabilities()
			c[0].Workloads = append(c[0].Workloads, GroupVersionKind{Group: "solti.io", Version: "v1", Kind: "Wasm"})
			a.SetCapabilities(c)
		}, true},
		{"runner labels removed", func(a *Agent) {
			c := a.Capabilities()
			c[1].Labels = nil
			a.SetCapabilities(c)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			earlier, later := base(), base()
			tc.change(later)
			if got := later.ChangedVisiblySince(earlier); got != tc.want {
				t.Fatalf("ChangedVisiblySince = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("no capabilities and nil capabilities are the same", func(t *testing.T) {
		earlier, later := base(), base()
		earlier.SetCapabilities(nil)
		later.SetCapabilities([]RunnerCapability{})
		if later.ChangedVisiblySince(earlier) {
			t.Fatal("empty and absent capabilities differ")
		}
	})
	t.Run("no earlier row", func(t *testing.T) {
		if !base().ChangedVisiblySince(nil) {
			t.Fatal("an agent with no earlier row is a visible change")
		}
	})
}
