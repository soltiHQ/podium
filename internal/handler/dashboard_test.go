package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
)

func TestDismissIssueRequiresSubjectEditPermission(t *testing.T) {
	for _, tc := range []struct {
		kind string
		need []enum.Permission
	}{
		{event.AgentInactive, []enum.Permission{enum.AgentsEdit}},
		{event.AgentDisconnected, []enum.Permission{enum.AgentsEdit}},
		{event.AgentDeleted, []enum.Permission{enum.AgentsEdit}},
		{event.SyncFailed, []enum.Permission{enum.SpecsEdit}},
		{event.RateLimited, []enum.Permission{enum.UsersEdit}},
		{"not_a_kind", []enum.Permission{enum.AgentsEdit, enum.SpecsEdit, enum.UsersEdit}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newAPIFixture(t)
			f.hub.Record(tc.kind, event.Payload{ID: "subject-1", Name: "subject-1"})
			issues := len(f.hub.RecentIssues(100))
			target := "/api/v1/dashboard/issues?kind=" + url.QueryEscape(tc.kind) + "&entity=subject-1"
			for _, missing := range tc.need {
				viewer := &identity.Identity{UserID: "viewer", Permissions: allExcept(missing)}
				if w := f.do(http.MethodDelete, target, "", viewer); w.Code != http.StatusForbidden {
					t.Fatalf("every permission but %s: %d, want 403", missing, w.Code)
				}
			}
			if got := len(f.hub.RecentIssues(100)); got != issues {
				t.Fatalf("refused dismissal left %d issues, want %d", got, issues)
			}
			if w := f.do(http.MethodDelete, target, "", &identity.Identity{UserID: "viewer", Permissions: tc.need}); w.Code != http.StatusNoContent {
				t.Fatalf("with %v: %d %s, want 204", tc.need, w.Code, w.Body.String())
			}
			if got := len(f.hub.RecentIssues(100)); got != 0 {
				t.Fatalf("dismissed issue still listed (%d)", got)
			}
		})
	}
}

// dashboardKinds returns the kinds of the feed and issue entries a viewer gets.
func dashboardKinds(t *testing.T, f *apiFixture, perms []enum.Permission) (events, issues []string) {
	t.Helper()
	w := f.do(http.MethodGet, "/api/v1/dashboard", "", &identity.Identity{UserID: "viewer", Permissions: perms})
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Events []struct{ Kind string }
		Issues []struct{ Kind string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, e := range body.Events {
		events = append(events, e.Kind)
	}
	for _, i := range body.Issues {
		issues = append(issues, i.Kind)
	}
	slices.Sort(events)
	slices.Sort(issues)
	return events, issues
}

func TestDashboardFeedFiltersByReadPermission(t *testing.T) {
	f := newAPIFixture(t)
	for _, kind := range []string{
		"agent_connected", "agent_inactive", "agent_disconnected", "agent_deleted",
		"spec_created", "spec_updated", "spec_deployed", "sync_failed",
		"user_created", "user_updated", "user_deleted", "user_password_changed", "user_status_changed",
		"session_created", "rate_limited", "issue_closed", "not_a_kind",
	} {
		f.hub.Record(kind, event.Payload{ID: "id-" + kind})
	}
	for _, tc := range []struct {
		name           string
		perms          []enum.Permission
		events, issues []string
	}{
		{
			"agents",
			[]enum.Permission{enum.AgentsGet},
			[]string{"agent_connected", "agent_deleted", "agent_disconnected", "agent_inactive"},
			[]string{"agent_deleted", "agent_disconnected", "agent_inactive"},
		},
		{
			"specs",
			[]enum.Permission{enum.SpecsGet},
			[]string{"spec_created", "spec_deployed", "spec_updated", "sync_failed"},
			[]string{"sync_failed"},
		},
		{
			"users",
			[]enum.Permission{enum.UsersGet},
			[]string{"rate_limited", "session_created", "user_created", "user_deleted", "user_password_changed", "user_status_changed", "user_updated"},
			[]string{"rate_limited"},
		},
		{
			"every subject",
			[]enum.Permission{enum.AgentsGet, enum.SpecsGet, enum.UsersGet},
			[]string{
				"agent_connected", "agent_deleted", "agent_disconnected", "agent_inactive", "issue_closed", "not_a_kind",
				"rate_limited", "session_created", "spec_created", "spec_deployed", "spec_updated", "sync_failed",
				"user_created", "user_deleted", "user_password_changed", "user_status_changed", "user_updated",
			},
			[]string{"agent_deleted", "agent_disconnected", "agent_inactive", "rate_limited", "sync_failed"},
		},
		{"edit without read", []enum.Permission{enum.AgentsEdit, enum.SpecsEdit, enum.UsersEdit}, nil, nil},
		{"none", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, issues := dashboardKinds(t, f, tc.perms)
			if !slices.Equal(events, tc.events) {
				t.Errorf("activity %v, want %v", events, tc.events)
			}
			if !slices.Equal(issues, tc.issues) {
				t.Errorf("issues %v, want %v", issues, tc.issues)
			}
		})
	}
}

// Logins are frequent; they must not push an agent-only viewer's activity
// out of the feed.
func TestDashboardFeedFillsFromOlderVisibleEvents(t *testing.T) {
	f := newAPIFixture(t)
	for range 40 {
		f.hub.Record(event.AgentConnected, event.Payload{ID: "agent-1"})
	}
	for range 40 {
		f.hub.Record(event.SessionCreated, event.Payload{ID: "user-1"})
	}
	events, _ := dashboardKinds(t, f, []enum.Permission{enum.AgentsGet})
	if len(events) != 30 || slices.ContainsFunc(events, func(k string) bool { return k != event.AgentConnected }) {
		t.Fatalf("agent viewer feed: %d entries %v, want 30 agent_connected", len(events), slices.Compact(events))
	}
}

func TestDashboardShowsDismissOnlyForIssuesTheViewerMayClose(t *testing.T) {
	issues := map[string]enum.Permission{
		event.AgentInactive: enum.AgentsEdit,
		event.SyncFailed:    enum.SpecsEdit,
		event.RateLimited:   enum.UsersEdit,
	}
	for _, edit := range [][]enum.Permission{
		nil,
		{enum.AgentsEdit},
		{enum.SpecsEdit},
		{enum.UsersEdit},
		{enum.AgentsEdit, enum.SpecsEdit, enum.UsersEdit},
	} {
		f := newAPIFixture(t)
		for kind := range issues {
			f.hub.Record(kind, event.Payload{ID: "entity-" + kind})
		}
		perms := append([]enum.Permission{enum.AgentsGet, enum.SpecsGet, enum.UsersGet}, edit...)
		page := f.render(t, "/api/v1/dashboard", &identity.Identity{UserID: "viewer", Permissions: perms})
		for kind, need := range issues {
			if !strings.Contains(page, "entity-"+kind) {
				t.Fatalf("with %v: %s issue not listed", edit, kind)
			}
			if shown, want := strings.Contains(page, "kind="+kind), slices.Contains(edit, need); shown != want {
				t.Errorf("with %v: dismiss for %s shown=%v, want %v", edit, kind, shown, want)
			}
		}
	}
}
