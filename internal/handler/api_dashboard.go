package handler

import (
	"net/http"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	contentHome "github.com/soltiHQ/control-plane/ui/templates/content/home"
)

const (
	// dashboardScan is how many recent events and issues the dashboard
	// filters; it matches the hub's ring capacity.
	dashboardScan = 100
	// dashboardFeed caps the activity feed after filtering.
	dashboardFeed = 30
)

// Dashboard handles GET /api/v1/dashboard.
func (a *API) Dashboard(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	if r.URL.Path != routepath.ApiDashboard {
		response.NotFound(w, r, mode)
		return
	}
	if r.Method != http.MethodGet {
		response.NotAllowed(w, r, mode)
		return
	}
	ctx := r.Context()
	viewer := a.identity(r)

	agents, err := a.agentSVC.List(ctx, agent.ListQuery{Limit: storage.MaxListLimit})
	if err != nil {
		a.logger.Error().Err(err).Msg("dashboard: agent list failed")
		response.Unavailable(w, r, mode)
		return
	}

	var active, inactive, disconnected int
	for _, ag := range agents.Items {
		switch ag.Status() {
		case enum.AgentStatusActive:
			active++
		case enum.AgentStatusInactive:
			inactive++
		case enum.AgentStatusDisconnected:
			disconnected++
		}
	}

	specs, err := a.specSVC.List(ctx, spec.ListQuery{Limit: storage.MaxListLimit})
	if err != nil {
		a.logger.Error().Err(err).Msg("dashboard: spec list failed")
		response.Unavailable(w, r, mode)
		return
	}

	users, err := a.userSVC.List(ctx, user.ListQuery{Limit: storage.MaxListLimit})
	if err != nil {
		a.logger.Error().Err(err).Msg("dashboard: user list failed")
		response.Unavailable(w, r, mode)
		return
	}

	rollouts, err := a.specSVC.Rollouts(ctx, storage.RolloutQueryCriteria{})
	if err != nil {
		a.logger.Error().Err(err).Msg("dashboard: rollouts list failed")
		response.Unavailable(w, r, mode)
		return
	}

	var synced, pending, failed, drift int
	for _, r := range rollouts {
		switch r.Status() {
		case enum.SyncStatusSynced:
			synced++
		case enum.SyncStatusPending:
			pending++
		case enum.SyncStatusFailed:
			failed++
		case enum.SyncStatusDrift:
			drift++
		}
	}

	stats := contentHome.DashboardStats{
		TotalAgents:   len(agents.Items),
		TotalSpecs:    len(specs.Items),
		TotalUsers:    len(users.Items),
		TotalRollouts: len(rollouts),

		ActiveAgents:       active,
		InactiveAgents:     inactive,
		DisconnectedAgents: disconnected,
		SyncedRollouts:     synced,
		PendingRollouts:    pending,
		FailedRollouts:     failed,
		DriftRollouts:      drift,

		Events: readable(viewer, a.hub.RecentEvents(dashboardScan), dashboardFeed),
		Issues: contentHome.GroupIssues(readable(viewer, a.hub.RecentIssues(dashboardScan), dashboardScan)),
	}
	response.OK(w, r, mode, &responder.View{
		Data:      stats,
		Component: contentHome.Dashboard(stats, policy.BuildDashboard(viewer)),
	})
}

// IssuesDelete handles DELETE /api/v1/dashboard/issues.
//
// Dismissing needs the edit permission of the issue's subject: agents:edit,
// taskspecs:edit or users:edit, and all three for a kind without a subject.
func (a *API) IssuesDelete(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	if r.Method != http.MethodDelete {
		response.NotAllowed(w, r, mode)
		return
	}

	kind := r.FormValue("kind")
	viewer := a.identity(r)
	if viewer == nil {
		response.Unauthorized(w, r, mode)
		return
	}
	if !hasAll(viewer, eventPermissions(kind, enum.AgentsEdit, enum.SpecsEdit, enum.UsersEdit)) {
		response.Forbidden(w, r, mode)
		return
	}
	entity := r.FormValue("entity")
	if kind == "" || entity == "" {
		response.BadRequestMsg(w, r, mode, "kind and entity are required")
		return
	}

	n := a.hub.DeleteIssues(kind, entity)
	if n > 0 {
		name := r.FormValue("name")
		if name == "" {
			name = entity
		}
		a.hub.Record(event.IssueClosed, event.Payload{
			By:   a.actor(r),
			Name: name,
		})
	}
	htmx.Trigger(w, event.RefreshDashboard)
	a.hub.Notify(event.RefreshDashboard)
	response.NoContent(w, r)
}

// eventPermissions returns the permissions that cover an event of the given
// kind: the one of its subject, or all three for a kind without one.
func eventPermissions(kind string, agents, specs, users enum.Permission) []enum.Permission {
	switch event.SubjectOf(kind) {
	case event.SubjectAgent:
		return []enum.Permission{agents}
	case event.SubjectSpec:
		return []enum.Permission{specs}
	case event.SubjectUser:
		return []enum.Permission{users}
	default:
		return []enum.Permission{agents, specs, users}
	}
}

// readable keeps, newest first, at most limit records the viewer may read.
func readable(viewer *identity.Identity, records []event.Record, limit int) []event.Record {
	var out []event.Record
	for _, rec := range records {
		if len(out) == limit {
			break
		}
		if hasAll(viewer, eventPermissions(rec.Kind, enum.AgentsGet, enum.SpecsGet, enum.UsersGet)) {
			out = append(out, rec)
		}
	}
	return out
}

func hasAll(id *identity.Identity, perms []enum.Permission) bool {
	for _, p := range perms {
		if !id.HasPermission(p) {
			return false
		}
	}
	return true
}
