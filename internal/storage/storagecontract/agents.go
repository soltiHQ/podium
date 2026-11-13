package storagecontract

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// agents checks the agent rows: the round trip, replacement, the rows a store
// refuses, deletion and the filter the factory builds.
func agents(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullAgent(t, "agent-a", 0, minute(0))
		must(t, s.UpsertAgent(ctx, want))

		got, err := s.GetAgent(ctx, want.ID())
		gotRow(t, "GetAgent", got, err, want, equalAgent)
		listed, err := listedAgents(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListAgents", listed, []*model.Agent{want}, equalAgent)
	})

	t.Run("upsert replaces", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		first, other := fullAgent(t, "agent-a", 0, minute(0)), fullAgent(t, "agent-b", 0, minute(3))
		must(t, errors.Join(s.UpsertAgent(ctx, first), s.UpsertAgent(ctx, other)))

		// Every field changes, the creation time too: the row lists at its new one,
		// and the filter sees its new name, not the old.
		want := fullAgent(t, "agent-a", 1, minute(5))
		must(t, s.UpsertAgent(ctx, want))
		got, err := s.GetAgent(ctx, want.ID())
		gotRow(t, "GetAgent", got, err, want, equalAgent)
		listed, err := listedAgents(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListAgents", listed, []*model.Agent{want, other}, equalAgent)
		listed, err = listedAgents(ctx, s, s.BuildAgentFilter(storage.AgentQueryCriteria{Query: want.Name()}))
		sameIDs(t, "ListAgents by the new name", listed, err, want.ID())
		listed, err = listedAgents(ctx, s, s.BuildAgentFilter(storage.AgentQueryCriteria{Query: first.Name()}))
		sameIDs(t, "ListAgents by the old name", listed, err)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.Agent{}
		noID.SetCreatedAt(minute(0))
		noCreation := fullAgent(t, "agent-a", 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"UpsertAgent of nil", s.UpsertAgent(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertAgent of a row without an ID", s.UpsertAgent(ctx, noID), storage.ErrInvalidArgument},
			outcome{"UpsertAgent of a row without a creation time", s.UpsertAgent(ctx, noCreation), storage.ErrInvalidArgument},
		)
		listed, err := listedAgents(ctx, s, nil)
		sameIDs(t, "ListAgents after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept := fullAgent(t, "agent-b", 0, minute(0))
		must(t, errors.Join(s.UpsertAgent(ctx, fullAgent(t, "agent-a", 0, minute(0))), s.UpsertAgent(ctx, kept)))
		expect(t,
			outcome{"DeleteAgent", s.DeleteAgent(ctx, "agent-a"), nil},
			outcome{"GetAgent of a deleted row", errOf(s.GetAgent(ctx, "agent-a")), storage.ErrNotFound},
			outcome{"DeleteAgent of a deleted row", s.DeleteAgent(ctx, "agent-a"), storage.ErrNotFound},
			outcome{"GetAgent of an ID never written", errOf(s.GetAgent(ctx, "agent-x")), storage.ErrNotFound},
			outcome{"DeleteAgent of an ID never written", s.DeleteAgent(ctx, "agent-x"), storage.ErrNotFound},
			outcome{"GetAgent of an empty ID", errOf(s.GetAgent(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteAgent of an empty ID", s.DeleteAgent(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetAgent(ctx, kept.ID())
		gotRow(t, "GetAgent of the row kept", got, err, kept, equalAgent)
	})

	t.Run("filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		bound := minute(10)
		for _, r := range []struct {
			id, name, endpoint string
			staleAt            time.Time
		}{
			{"agent-kilo", "Lima", "https://mike.zone1.example.net:8443", bound.Add(-time.Nanosecond)},
			{"agent-oscar", "papa_quebec", "https://romeo.zone1.example.net:8443", bound},
			{"agent-sierra", "tango 100%", "https://uniform.zone2.example.net:8443", time.Time{}},
			{"agent-victor", "papaxquebec", "https://whiskey.zone2.example.net:8443", bound.Add(time.Minute)},
		} {
			p := agentParams(r.id, 0)
			p.Name, p.Endpoint = r.name, r.endpoint
			a := newAgent(t, p, 0, minute(0))
			a.SetStaleAt(r.staleAt)
			must(t, s.UpsertAgent(ctx, a))
		}
		every := []string{"agent-kilo", "agent-oscar", "agent-sierra", "agent-victor"}
		listed, err := listedAgents(ctx, s, nil)
		sameIDs(t, "ListAgents with no filter", listed, err, every...)
		for _, tc := range []struct {
			name string
			c    storage.AgentQueryCriteria
			want []string
		}{
			{"empty criteria", storage.AgentQueryCriteria{}, every},
			{"query on the ID, in another case", storage.AgentQueryCriteria{Query: "KILO"}, []string{"agent-kilo"}},
			{"query on the name, in another case", storage.AgentQueryCriteria{Query: "lIMA"}, []string{"agent-kilo"}},
			{"query on the endpoint, in another case", storage.AgentQueryCriteria{Query: "MIKE"}, []string{"agent-kilo"}},
			{"query matching two rows", storage.AgentQueryCriteria{Query: "zone1"}, []string{"agent-kilo", "agent-oscar"}},
			// The spec is silent on spaces around a query: the in-memory store trims
			// them, so a query of spaces alone is no query.
			{"query with spaces around it", storage.AgentQueryCriteria{Query: "  kilo  "}, []string{"agent-kilo"}},
			{"query of spaces", storage.AgentQueryCriteria{Query: "   "}, every},
			{"query on a field not searched", storage.AgentQueryCriteria{Query: "linux"}, nil},
			{"query with an underscore, matched as one", storage.AgentQueryCriteria{Query: "papa_q"}, []string{"agent-oscar"}},
			{"query with a percent sign, matched as one", storage.AgentQueryCriteria{Query: "%"}, []string{"agent-sierra"}},
			// A deadline before the bound, by a nanosecond, matches, and so does one
			// never set; one at the bound or after it does not.
			{"stale before", storage.AgentQueryCriteria{StaleAtBefore: bound}, []string{"agent-kilo", "agent-sierra"}},
			{"query and stale before", storage.AgentQueryCriteria{Query: "zone1", StaleAtBefore: bound}, []string{"agent-kilo"}},
		} {
			listed, err := listedAgents(ctx, s, s.BuildAgentFilter(tc.c))
			sameIDs(t, "ListAgents, "+tc.name, listed, err, tc.want...)
		}
	})

	t.Run("foreign filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, s.UpsertAgent(ctx, fullAgent(t, "agent-a", 0, minute(0))))
		expect(t,
			outcome{"ListAgents with a filter of no backend", errOf(s.ListAgents(ctx, foreignFilter{}, storage.ListOptions{})), storage.ErrInvalidArgument},
			outcome{"ListAgents with a user filter", errOf(s.ListAgents(ctx, s.BuildUserFilter(storage.UserQueryCriteria{}), storage.ListOptions{})), storage.ErrInvalidArgument},
		)
	})
}

// listedAgents drains ListAgents with filter.
func listedAgents(ctx context.Context, s storage.Storage, filter storage.AgentFilter) ([]*model.Agent, error) {
	return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.AgentListResult, error) {
		return s.ListAgents(ctx, filter, o)
	})
}

// agentParams is the discovery payload of agent id. Its every field is set from
// v, so that payloads of two values of v differ in every field; an even v gives
// an HTTP endpoint, an odd one a gRPC endpoint.
func agentParams(id string, v int) model.AgentParams {
	p := model.AgentParams{
		ID:                 id,
		Name:               fmt.Sprintf("edge %s v%d", id, v),
		UptimeSeconds:      int64(3600 * (v + 1)),
		HeartbeatIntervalS: 15 * (v + 1),
		APIVersion:         1 - v%2, // v1, or unspecified: the only other value
		OS:                 fmt.Sprintf("linux-%d", v),
		Arch:               []string{"arm64", "amd64"}[v%2],
		Platform:           fmt.Sprintf("ubuntu-24.0%d", v),
		Metadata:           map[string]string{"hostname": fmt.Sprintf("%s.dc%d.example.net", id, v), "kernel": fmt.Sprintf("6.8.%d", v)},
		Capabilities: []model.RunnerCapability{
			{
				Name:      fmt.Sprintf("subprocess-%d", v),
				Labels:    map[string]string{"pool": fmt.Sprintf("general-%d", v)},
				Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: fmt.Sprintf("v%d", v+1), Kind: "Subprocess"}},
			},
			{
				Name:   fmt.Sprintf("container-%d", v),
				Labels: map[string]string{"pool": fmt.Sprintf("gpu-%d", v), "zone": fmt.Sprintf("z%d", v)},
				Workloads: []model.GroupVersionKind{
					{Group: "solti.io", Version: fmt.Sprintf("v%d", v+1), Kind: "Container"},
					{Group: "wasm.solti.io", Version: fmt.Sprintf("v%dbeta", v+1), Kind: "Module"},
				},
			},
		},
	}
	if v%2 == 0 {
		p.EndpointType, p.Endpoint = 2, fmt.Sprintf("https://%s.example.net:%d/v%d", id, 8443+v, v)
	} else {
		p.EndpointType, p.Endpoint = 1, fmt.Sprintf("%s.example.net:%d", id, 9443+v)
	}
	return p
}

// newAgent builds the agent of p and sets the fields discovery does not carry
// from v, created at created.
func newAgent(t *testing.T, p model.AgentParams, v int, created time.Time) *model.Agent {
	t.Helper()
	a, err := model.NewAgentFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	a.SetLabels(map[string]string{"env": fmt.Sprintf("prod-%d", v), "team": fmt.Sprintf("edge-%d", v)})
	a.SetStatus([]enum.AgentStatus{enum.AgentStatusInactive, enum.AgentStatusDisconnected}[v%2])
	a.SetLastSeenAt(created.Add(time.Duration(v+1) * time.Minute))
	a.SetStaleAt(created.Add(time.Duration(v+2)*time.Minute + 30*time.Second))
	a.SetCreatedAt(created)
	a.SetUpdatedAt(created.Add(time.Duration(v+3) * time.Minute))
	return a
}

// fullAgent is agent id with every field set from v: with v 0, none to its zero
// or default value.
func fullAgent(t *testing.T, id string, v int, created time.Time) *model.Agent {
	t.Helper()
	return newAgent(t, agentParams(id, v), v, created)
}

// equalAgent compares every accessor of an agent read back with the agent written.
func equalAgent(t *testing.T, what string, got, want *model.Agent) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil agent, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("Name", got.Name(), want.Name())
	f.same("Endpoint", got.Endpoint(), want.Endpoint())
	f.same("EndpointType", got.EndpointType(), want.EndpointType())
	f.same("APIVersion", got.APIVersion(), want.APIVersion())
	f.same("UptimeSeconds", got.UptimeSeconds(), want.UptimeSeconds())
	f.same("OS", got.OS(), want.OS())
	f.same("Arch", got.Arch(), want.Arch())
	f.same("Platform", got.Platform(), want.Platform())
	f.same("Capabilities", got.Capabilities(), want.Capabilities())
	f.same("Status", got.Status(), want.Status())
	f.same("HeartbeatInterval", got.HeartbeatInterval(), want.HeartbeatInterval())
	f.same("MetadataAll", got.MetadataAll(), want.MetadataAll())
	f.same("LabelsAll", got.LabelsAll(), want.LabelsAll())
	f.instant("LastSeenAt", got.LastSeenAt(), want.LastSeenAt())
	f.instant("StaleAt", got.StaleAt(), want.StaleAt())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
