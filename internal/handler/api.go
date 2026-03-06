package handler

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/segmentio/ksuid"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
	"github.com/soltiHQ/control-plane/internal/uikit/trigger"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/session"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	apimapv1 "github.com/soltiHQ/control-plane/internal/transport/http/apimap/v1"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transportctx"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	contentAgent "github.com/soltiHQ/control-plane/ui/templates/content/agent"
	contentHome "github.com/soltiHQ/control-plane/ui/templates/content/home"
	contentSpec "github.com/soltiHQ/control-plane/ui/templates/content/spec"
	contentUser "github.com/soltiHQ/control-plane/ui/templates/content/user"
)

// userStatusMode selects enable/disable branch in userSetStatus.
type userStatusMode uint8

const (
	userDisable userStatusMode = iota
	userActive
)

// upsertMode selects create/update branch in upsert handlers.
type upsertMode uint8

const (
	modeCreate upsertMode = iota
	modeUpdate
)

// API handlers.
type API struct {
	credentialSVC *credential.Service
	sessionSVC    *session.Service
	accessSVC     *access.Service
	agentSVC      *agent.Service
	specSVC       *spec.Service
	userSVC       *user.Service
	proxyPool     *proxy.Pool

	logger zerolog.Logger
}

// NewAPI creates a new API handler.
func NewAPI(
	logger zerolog.Logger,
	userSVC *user.Service,
	accessSVC *access.Service,
	sessionSVC *session.Service,
	credentialSVC *credential.Service,
	agentSVC *agent.Service,
	specSVC *spec.Service,
	proxyPool *proxy.Pool,
) *API {
	if accessSVC == nil {
		panic("handler.API: accessSVC is nil")
	}
	if userSVC == nil {
		panic("handler.API: userSVC is nil")
	}
	if sessionSVC == nil {
		panic("handler.API: sessionSVC is nil")
	}
	if credentialSVC == nil {
		panic("handler.API: credentialSVC is nil")
	}
	if agentSVC == nil {
		panic("handler.API: agentSVC is nil")
	}
	if specSVC == nil {
		panic("handler.API: specSVC is nil")
	}
	if proxyPool == nil {
		panic("handler.API: proxyPool is nil")
	}
	return &API{
		logger: logger.With().Str("handler", "api").Logger(),

		credentialSVC: credentialSVC,
		sessionSVC:    sessionSVC,
		accessSVC:     accessSVC,
		agentSVC:      agentSVC,
		specSVC:       specSVC,
		userSVC:       userSVC,
		proxyPool:     proxyPool,
	}
}

// Routes registers API routes.
// Auth runs at mux level once. Permissions are enforced per-method/per-subroute below.
func (a *API) Routes(mux *http.ServeMux, auth route.BaseMW, _ route.PermMW, common ...route.BaseMW) {
	route.HandleFunc(mux, routepath.ApiUsers, a.Users, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiUser, a.UsersRouter, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiSession, a.SessionsRouter, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiAgents, a.Agents, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiAgent, a.AgentsRouter, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiSpecs, a.Specs, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiSpec, a.SpecsRouter, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiDashboard, a.Dashboard, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiPermissions, a.Permissions, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiRoles, a.Roles, append(common, auth)...)
}

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

	agents, err := a.agentSVC.List(ctx, agent.ListQuery{Limit: storage.MaxListLimit})
	if err != nil {
		a.logger.Error().Err(err).Msg("dashboard: agent list failed")
		response.Unavailable(w, r, mode)
		return
	}

	var active, inactive, disconnected int
	for _, ag := range agents.Items {
		switch ag.Status() {
		case kind.AgentStatusActive:
			active++
		case kind.AgentStatusInactive:
			inactive++
		case kind.AgentStatusDisconnected:
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

	rollouts, err := a.specSVC.Rollouts(ctx, nil)
	if err != nil {
		a.logger.Error().Err(err).Msg("dashboard: rollouts list failed")
		response.Unavailable(w, r, mode)
		return
	}

	var synced, pending, failed, drift int
	for _, r := range rollouts {
		switch r.Status() {
		case kind.SyncStatusSynced:
			synced++
		case kind.SyncStatusPending:
			pending++
		case kind.SyncStatusFailed:
			failed++
		case kind.SyncStatusDrift:
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

		Events: trigger.RecentEvents(30),
		Issues: trigger.RecentEventsOfKind(15,
			trigger.EventAgentDisconnected,
			trigger.EventAgentInactive,
			trigger.EventAgentDeleted,
			trigger.EventRateLimited,
		),
	}
	response.OK(w, r, mode, &responder.View{
		Data:      stats,
		Component: contentHome.Dashboard(stats),
	})
}

// Users handles /api/v1/users.
//
// Supported:
//   - GET  /api/v1/users
//   - POST /api/v1/users
func (a *API) Users(w http.ResponseWriter, r *http.Request) {
	a.resource(w, r, routepath.ApiUsers,
		endpoint{http.MethodGet, kind.UsersGet, a.userList},
		endpoint{http.MethodPost, kind.UsersAdd, func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode) {
			a.userUpsert(w, r, m, "", modeCreate)
		}},
	)
}

// UsersRouter handles /api/v1/users/{id} and subroutes.
//
// Supported:
//   - GET    /api/v1/users/{id}
//   - PUT    /api/v1/users/{id}
//   - DELETE /api/v1/users/{id}
//   - GET    /api/v1/users/{id}/sessions
//   - POST   /api/v1/users/{id}/disable
//   - POST   /api/v1/users/{id}/enable
//   - POST   /api/v1/users/{id}/password
func (a *API) UsersRouter(w http.ResponseWriter, r *http.Request) {
	a.router(w, r, routepath.ApiUser,
		subroute{"", http.MethodGet, kind.UsersGet, a.usersDetails},
		subroute{"", http.MethodPut, kind.UsersEdit, func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode, id string) {
			a.userUpsert(w, r, m, id, modeUpdate)
		}},
		subroute{"", http.MethodDelete, kind.UsersDelete, a.userDelete},
		subroute{"sessions", http.MethodGet, kind.UsersGet, a.usersSessions},
		subroute{"disable", http.MethodPost, kind.UsersEdit, func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode, id string) {
			a.userSetStatus(w, r, m, id, userDisable)
		}},
		subroute{"enable", http.MethodPost, kind.UsersEdit, func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode, id string) {
			a.userSetStatus(w, r, m, id, userActive)
		}},
		subroute{"password", http.MethodPost, kind.UsersEdit, a.userSetPassword},
	)
}

// SessionsRouter handles /api/v1/sessions/{id} action subroutes.
//
// Supported:
//   - POST /api/v1/sessions/{sessionID}/revoke
func (a *API) SessionsRouter(w http.ResponseWriter, r *http.Request) {
	a.router(w, r, routepath.ApiSession,
		subroute{"revoke", http.MethodPost, kind.UsersEdit, a.userRevokeSession},
	)
}

// Agents handles /api/v1/agents.
//
// Supported:
//   - GET /api/v1/agents
func (a *API) Agents(w http.ResponseWriter, r *http.Request) {
	a.resource(w, r, routepath.ApiAgents,
		endpoint{http.MethodGet, kind.AgentsGet, a.agentList},
	)
}

// AgentsRouter handles /api/v1/agents/{id} and subroutes.
//
// Supported:
//   - GET  /api/v1/agents/{id}
//   - PUT  /api/v1/agents/{id}/labels
//   - GET  /api/v1/agents/{id}/tasks
func (a *API) AgentsRouter(w http.ResponseWriter, r *http.Request) {
	a.router(w, r, routepath.ApiAgent,
		subroute{"", http.MethodGet, kind.AgentsGet, a.agentDetails},
		subroute{"labels", http.MethodPut, kind.AgentsEdit, a.agentPatchLabels},
		subroute{"tasks", http.MethodGet, kind.AgentsGet, a.agentTasksList},
	)
}

// Permissions handles /api/v1/permissions.
//
// Supported:
//   - GET /api/v1/permissions
func (a *API) Permissions(w http.ResponseWriter, r *http.Request) {
	a.resource(w, r, "",
		endpoint{http.MethodGet, kind.UsersEdit, a.permissionsList},
	)
}

// Roles handles /api/v1/roles.
//
// Supported:
//   - GET /api/v1/roles
func (a *API) Roles(w http.ResponseWriter, r *http.Request) {
	a.resource(w, r, "",
		endpoint{http.MethodGet, kind.UsersEdit, a.rolesList},
	)
}

func (a *API) permissionsList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	perms := a.accessSVC.GetPermissions()

	items := make([]string, 0, len(perms))
	for _, p := range perms {
		items = append(items, apimapv1.Permission(p))
	}
	response.OK(w, r, mode, &responder.View{
		Data: restv1.PermissionListResponse{Items: items},
	})
}

func (a *API) rolesList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	roles, err := a.accessSVC.GetRoles(r.Context())
	if err != nil {
		a.logger.Error().Err(err).Msg("roles list failed")
		response.Unavailable(w, r, mode)
		return
	}

	items := mapSlice(roles, apimapv1.Role)
	response.OK(w, r, mode, &responder.View{
		Data: restv1.RoleListResponse{Items: items},
	})
}

func (a *API) agentList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	var (
		limit  = queryInt(r, "limit", 0)
		filter storage.AgentFilter

		cursor = r.URL.Query().Get("cursor")
		q      = r.URL.Query().Get("q")
	)
	if q != "" {
		filter = inmemory.NewAgentFilter().Query(q)
	}

	res, err := a.agentSVC.List(r.Context(), agent.ListQuery{
		Limit:  limit,
		Cursor: cursor,
		Filter: filter,
	})
	if err != nil {
		a.logger.Error().Err(err).Msg("agent list failed")
		response.Unavailable(w, r, mode)
		return
	}

	items := mapSlice(res.Items, apimapv1.Agent)
	response.OK(w, r, mode, &responder.View{
		Data: restv1.AgentListResponse{
			Items:      items,
			NextCursor: res.NextCursor,
		},
		Component: contentAgent.List(res.Items, res.NextCursor, q),
	})
}

func (a *API) agentDetails(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	ag, err := a.agentSVC.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		a.logger.Error().Err(err).Str("agent_id", id).Msg("agent get failed")
		response.Unavailable(w, r, mode)
		return
	}

	apiAgent := apimapv1.Agent(ag)
	response.OK(w, r, mode, &responder.View{
		Data:      apiAgent,
		Component: contentAgent.Detail(apiAgent, policy.BuildAgentDetail(a.identity(r))),
	})
}

func (a *API) agentPatchLabels(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	labels, err := decodeJSON[map[string]string](r)
	if err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	_, err = a.agentSVC.PatchLabels(r.Context(), agent.PatchLabels{
		ID:     id,
		Labels: labels,
	})
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			a.logger.Warn().Str("agent_id", id).Msg("agent not found")
			response.NotFound(w, r, mode)
			return
		}
		a.logger.Error().Err(err).Str("agent_id", id).Msg("agent patch labels failed")
		response.Unavailable(w, r, mode)
		return
	}

	a.logger.Info().Str("agent_id", id).Msg("agent labels updated")
	trigger.Set(w, trigger.AgentUpdate)
	response.NoContent(w, r)
}

// TODO: remove "q" - need to understand a correct way for getting tasks from agent with paginator and etc.
func (a *API) agentTasksList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, agentID string) {
	ag, err := a.agentSVC.Get(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		response.Unavailable(w, r, mode)
		return
	}
	if ag.Endpoint() == "" {
		response.Unavailable(w, r, mode)
		return
	}

	var (
		filter = proxy.TaskFilter{
			Status: r.URL.Query().Get("status"),
			Limit:  queryInt(r, "limit", 0),
			Offset: queryInt(r, "offset", 0),
		}
		q = r.URL.Query().Get("q")
	)

	p, err := a.proxyPool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion())
	if err != nil {
		a.logger.Error().Err(err).
			Str("agent_id", agentID).
			Str("endpoint", ag.Endpoint()).
			Msg("proxy: pool get failed")
		switch {
		case errors.Is(err, proxy.ErrUnsupportedAPIVersion),
			errors.Is(err, proxy.ErrUnsupportedEndpointType):
			response.BadRequest(w, r, mode)
		default:
			response.Unavailable(w, r, mode)
		}
		return
	}

	result, err := p.ListTasks(r.Context(), filter)
	if err != nil {
		a.logger.Warn().Err(err).
			Str("agent_id", agentID).
			Str("endpoint", ag.Endpoint()).
			Msg("proxy: ListTasks failed")
		response.Unavailable(w, r, mode)
		return
	}
	if q != "" {
		lower := strings.ToLower(q)
		filtered := result.Tasks[:0]
		for _, t := range result.Tasks {
			if strings.Contains(strings.ToLower(t.Slot), lower) ||
				strings.Contains(strings.ToLower(t.ID), lower) {
				filtered = append(filtered, t)
			}
		}
		result.Tasks = filtered
		result.Total = len(filtered)
	}
	slices.SortFunc(result.Tasks, func(a, b proxyv1.Task) int {
		return cmp.Compare(kind.ParseTaskStatus(a.Status).Priority(), kind.ParseTaskStatus(b.Status).Priority())
	})
	response.OK(w, r, mode, &responder.View{
		Data:      result,
		Component: contentAgent.Tasks(agentID, result.Tasks, result.Total, q, filter.Offset),
	})
}

func (a *API) userList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	var (
		limit  = queryInt(r, "limit", 0)
		filter storage.UserFilter

		cursor = r.URL.Query().Get("cursor")
		q      = r.URL.Query().Get("q")
	)
	if q != "" {
		filter = inmemory.NewUserFilter().Query(q)
	}

	res, err := a.userSVC.List(r.Context(), user.ListQuery{
		Limit:  limit,
		Cursor: cursor,
		Filter: filter,
	})
	if err != nil {
		a.logger.Error().Err(err).Msg("user list failed")
		response.Unavailable(w, r, mode)
		return
	}

	items := mapSlice(res.Items, apimapv1.User)
	response.OK(w, r, mode, &responder.View{
		Data: restv1.UserListResponse{
			Items:      items,
			NextCursor: res.NextCursor,
		},
		Component: contentUser.List(res.Items, res.NextCursor, q),
	})
}

func (a *API) usersDetails(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	u, err := a.userSVC.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		a.logger.Error().Err(err).Str("user_id", id).Msg("user get failed")
		response.Unavailable(w, r, mode)
		return
	}

	apiUser := apimapv1.User(u)
	response.OK(w, r, mode, &responder.View{
		Data:      apiUser,
		Component: contentUser.Detail(apiUser, policy.BuildUserDetail(a.identity(r), id)),
	})
}

func (a *API) usersSessions(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	res, err := a.sessionSVC.ListByUser(r.Context(), session.ListByUserQuery{UserID: id})
	if err != nil {
		a.logger.Error().Err(err).Str("user_id", id).Msg("user sessions list failed")
		response.Unavailable(w, r, mode)
		return
	}

	items := mapSlice(res.Items, apimapv1.Session)
	slices.SortFunc(items, func(a, b restv1.Session) int {
		pa := kind.DeriveSessionStatus(a.Revoked, a.ExpiresAt).Priority()
		pb := kind.DeriveSessionStatus(b.Revoked, b.ExpiresAt).Priority()
		if pa != pb {
			return cmp.Compare(pa, pb)
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	response.OK(w, r, mode, &responder.View{
		Data:      restv1.SessionResponse{Items: items},
		Component: contentUser.Sessions(items),
	})
}

func (a *API) userUpsert(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string, action upsertMode) {
	in, err := decodeJSON[restv1.User](r)
	if err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	var u *model.User

	switch action {
	case modeCreate:
		if in.Subject == "" {
			response.BadRequest(w, r, mode)
			return
		}
		x, err := model.NewUser(ksuid.New().String(), in.Subject)
		if err != nil {
			response.BadRequest(w, r, mode)
			return
		}
		x.Enable()
		u = x
	case modeUpdate:
		if id == "" || (in.ID != "" && in.ID != id) {
			response.BadRequest(w, r, mode)
			return
		}
		x, err := a.userSVC.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				response.NotFound(w, r, mode)
				return
			}
			a.logger.Error().Err(err).Str("user_id", id).Msg("user get failed")
			response.Unavailable(w, r, mode)
			return
		}
		u = x
	default:
		response.BadRequest(w, r, mode)
		return
	}

	if in.Name != "" {
		u.NameAdd(in.Name)
	}
	if in.Email != "" {
		u.EmailAdd(in.Email)
	}
	if in.Subject != "" {
		u.SubjectAdd(in.Subject)
	}
	if len(in.RoleIDs) > 0 {
		u.RolesIDsNew(in.RoleIDs)
	}
	if len(in.Permissions) > 0 {
		u.PermissionsNew(in.Permissions)
	}
	if err = a.userSVC.Upsert(r.Context(), u); err != nil {
		a.logger.Error().Err(err).Str("user_id", u.ID()).Msg("user upsert failed")
		response.Unavailable(w, r, mode)
		return
	}

	by := a.actor(r)

	if action == modeCreate {
		a.logger.Info().Str("user_id", u.ID()).Str("subject", u.Subject()).Msg("user created")
		trigger.Record(trigger.EventUserCreated, trigger.EventPayload{
			ID: u.ID(), Name: u.Name(), By: by,
		})
		trigger.Notify(trigger.UserUpdate)
		trigger.Redirect(w, routepath.PageUsers)
		response.NoContent(w, r)
		return
	}
	a.logger.Info().Str("user_id", id).Msg("user updated")
	trigger.Record(trigger.EventUserUpdated, trigger.EventPayload{
		ID: u.ID(), Name: u.Name(), By: by,
	})
	trigger.Set(w, trigger.UserUpdate)
	response.NoContent(w, r)
}

func (a *API) userDelete(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	var name string
	if u, err := a.userSVC.Get(r.Context(), id); err == nil {
		name = u.Name()
	}

	err := a.userSVC.Delete(r.Context(), id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		a.logger.Error().Err(err).Str("user_id", id).Msg("user delete failed")
		response.Unavailable(w, r, mode)
		return
	}
	a.logger.Info().Str("user_id", id).Msg("user deleted")
	trigger.Record(trigger.EventUserDeleted, trigger.EventPayload{
		ID: id, Name: name, By: a.actor(r),
	})
	trigger.Notify(trigger.UserUpdate)
	trigger.Redirect(w, routepath.PageUsers)
	response.NoContent(w, r)
}

func (a *API) userSetStatus(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, userID string, status userStatusMode) {
	u, err := a.userSVC.Get(r.Context(), userID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		a.logger.Error().Err(err).Str("user_id", userID).Msg("user get failed")
		response.Unavailable(w, r, mode)
		return
	}

	if status == userActive {
		u.Enable()
	} else {
		u.Disable()
	}

	if err = a.userSVC.Upsert(r.Context(), u); err != nil {
		a.logger.Error().Err(err).Str("user_id", userID).Msg("user status update failed")
		response.Unavailable(w, r, mode)
		return
	}
	a.logger.Info().Str("user_id", userID).Msg("user status changed")
	detail := "inactive"
	if status == userActive {
		detail = "active"
	}
	trigger.Record(trigger.EventUserStatusChanged, trigger.EventPayload{
		ID: u.ID(), Name: u.Name(), By: a.actor(r), Detail: detail,
	})
	trigger.Set(w, trigger.UserUpdate)
	response.NoContent(w, r)
}

func (a *API) userSetPassword(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, userID string) {
	in, err := decodeJSON[restv1.SetPasswordRequest](r)
	if err != nil {
		response.BadRequest(w, r, mode)
		return
	}
	if in.Password == "" {
		response.BadRequest(w, r, mode)
		return
	}

	err = a.credentialSVC.SetPassword(r.Context(), credential.SetPasswordRequest{
		UserID:   userID,
		Password: in.Password,
	})
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrNotFound):
			a.logger.Warn().Str("user_id", userID).Msg("password change: user not found")
		case errors.Is(err, auth.ErrUserDisabled):
			a.logger.Warn().Str("user_id", userID).Msg("password change: user is disabled")
		default:
			a.logger.Error().Err(err).Str("user_id", userID).Msg("password change failed")
		}
		response.Unavailable(w, r, mode)
		return
	}

	a.logger.Info().Str("user_id", userID).Msg("user password changed")
	var userName string
	if u, err := a.userSVC.Get(r.Context(), userID); err == nil {
		userName = u.Name()
	}
	trigger.Record(trigger.EventUserPasswordChanged, trigger.EventPayload{
		ID: userID, Name: userName, By: a.actor(r),
	})
	trigger.Set(w, trigger.UserUpdate)
	response.NoContent(w, r)
}

func (a *API) userRevokeSession(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	err := a.sessionSVC.Revoke(
		r.Context(),
		session.RevokeRequest{ID: id, At: time.Now()},
	)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		a.logger.Error().Err(err).Str("session_id", id).Msg("session revoke failed")
		response.Unavailable(w, r, mode)
		return
	}

	a.logger.Info().Str("session_id", id).Msg("session revoked")
	trigger.Set(w, trigger.SessionUpdate)
	response.NoContent(w, r)
}

// Specs handles /api/v1/specs.
//
// Supported:
//   - GET  /api/v1/specs
//   - POST /api/v1/specs
func (a *API) Specs(w http.ResponseWriter, r *http.Request) {
	a.resource(w, r, routepath.ApiSpecs,
		endpoint{http.MethodGet, kind.SpecsGet, a.specList},
		endpoint{http.MethodPost, kind.SpecsAdd, func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode) {
			a.specUpsert(w, r, m, "", modeCreate)
		}},
	)
}

// SpecsRouter handles /api/v1/specs/{id} and subroutes.
//
// Supported:
//   - GET    /api/v1/specs/{id}
//   - PUT    /api/v1/specs/{id}
//   - DELETE /api/v1/specs/{id}
//   - POST   /api/v1/specs/{id}/deploy
//   - GET    /api/v1/specs/{id}/sync
func (a *API) SpecsRouter(w http.ResponseWriter, r *http.Request) {
	a.router(w, r, routepath.ApiSpec,
		subroute{"", http.MethodGet, kind.SpecsGet, a.specDetails},
		subroute{"", http.MethodPut, kind.SpecsEdit, func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode, id string) {
			a.specUpsert(w, r, m, id, modeUpdate)
		}},
		subroute{"", http.MethodDelete, kind.SpecsEdit, a.specDelete},
		subroute{"deploy", http.MethodPost, kind.SpecsDeploy, a.specDeploy},
		subroute{"sync", http.MethodGet, kind.SpecsGet, a.specRollouts},
	)
}

func (a *API) specList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	var (
		limit  = queryInt(r, "limit", 0)
		filter storage.SpecFilter

		cursor = r.URL.Query().Get("cursor")
		q      = r.URL.Query().Get("q")
	)
	if q != "" {
		filter = inmemory.NewSpecFilter().Query(q)
	}

	res, err := a.specSVC.List(r.Context(), spec.ListQuery{
		Limit:  limit,
		Cursor: cursor,
		Filter: filter,
	})
	if err != nil {
		a.logger.Error().Err(err).Msg("spec list failed")
		response.Unavailable(w, r, mode)
		return
	}

	items := mapSlice(res.Items, apimapv1.Spec)
	response.OK(w, r, mode, &responder.View{
		Data: restv1.SpecListResponse{
			Items:      items,
			NextCursor: res.NextCursor,
		},
		Component: contentSpec.List(res.Items, res.NextCursor, q),
	})
}

func (a *API) specDetails(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	ts, err := a.specSVC.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		a.logger.Error().Err(err).Str("spec", id).Msg("spec get failed")
		response.Unavailable(w, r, mode)
		return
	}

	states, err := a.specSVC.RolloutsBySpec(r.Context(), id, inmemory.NewRolloutFilter().BySpecID(id))
	if err != nil {
		a.logger.Error().Err(err).Str("spec", id).Msg("spec rollouts failed")
		response.Unavailable(w, r, mode)
		return
	}

	dto := apimapv1.RolloutSpec(ts, states)
	response.OK(w, r, mode, &responder.View{
		Data:      dto,
		Component: contentSpec.Detail(dto, policy.BuildSpecDetail(a.identity(r))),
	})
}

func (a *API) specUpsert(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string, action upsertMode) {
	in, err := decodeJSON[restv1.SpecCreateRequest](r)
	if err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	var ts *model.Spec

	switch action {
	case modeCreate:
		if in.Name == "" || in.Slot == "" {
			response.BadRequest(w, r, mode)
			return
		}
		x, err := model.NewSpec(ksuid.New().String(), in.Name, in.Slot)
		if err != nil {
			response.BadRequest(w, r, mode)
			return
		}
		ts = x
	case modeUpdate:
		x, err := a.specSVC.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				response.NotFound(w, r, mode)
				return
			}
			a.logger.Error().Err(err).Str("spec", id).Msg("spec get failed")
			response.Unavailable(w, r, mode)
			return
		}
		if in.Name != "" {
			x.SetName(in.Name)
		}
		if in.Slot != "" {
			x.SetSlot(in.Slot)
		}
		ts = x
	default:
		response.BadRequest(w, r, mode)
		return
	}

	// Kind
	if in.KindType != "" {
		ts.SetKindType(kind.TaskKindType(in.KindType))
	}
	if in.KindConfig != nil {
		ts.SetKindConfig(in.KindConfig)
	}

	// Lifecycle
	if in.TimeoutMs > 0 {
		ts.SetTimeoutMs(in.TimeoutMs)
	}
	if in.RestartType != "" {
		ts.SetRestartType(kind.RestartType(in.RestartType))
	}
	if in.IntervalMs > 0 {
		ts.SetIntervalMs(in.IntervalMs)
	}
	if action == modeCreate {
		ts.SetBackoff(model.BackoffConfig{
			Jitter:  kind.JitterStrategy(in.Jitter),
			FirstMs: in.BackoffFirstMs,
			MaxMs:   in.BackoffMaxMs,
			Factor:  in.BackoffFactor,
		})
	} else if in.Jitter != "" || in.BackoffFirstMs > 0 || in.BackoffMaxMs > 0 || in.BackoffFactor > 0 {
		b := ts.Backoff()
		if in.Jitter != "" {
			b.Jitter = kind.JitterStrategy(in.Jitter)
		}
		if in.BackoffFirstMs > 0 {
			b.FirstMs = in.BackoffFirstMs
		}
		if in.BackoffMaxMs > 0 {
			b.MaxMs = in.BackoffMaxMs
		}
		if in.BackoffFactor > 0 {
			b.Factor = in.BackoffFactor
		}
		ts.SetBackoff(b)
	}
	if in.Admission != "" {
		ts.SetAdmission(kind.AdmissionStrategy(in.Admission))
	}

	// Targets
	if action == modeCreate {
		if len(in.Targets) > 0 {
			ts.SetTargets(in.Targets)
		}
		if len(in.TargetLabels) > 0 {
			ts.SetTargetLabels(in.TargetLabels)
		}
		if len(in.RunnerLabels) > 0 {
			ts.SetRunnerLabels(in.RunnerLabels)
		}
	} else {
		if in.Targets != nil {
			ts.SetTargets(in.Targets)
		}
		if in.TargetLabels != nil {
			ts.SetTargetLabels(in.TargetLabels)
		}
		if in.RunnerLabels != nil {
			ts.SetRunnerLabels(in.RunnerLabels)
		}
	}

	if action == modeCreate {
		if err := a.specSVC.Create(r.Context(), ts); err != nil {
			a.logger.Error().Err(err).Msg("spec create failed")
			response.Unavailable(w, r, mode)
			return
		}
		a.logger.Info().Str("spec", ts.ID()).Str("name", ts.Name()).Msg("spec created")
		trigger.Record(trigger.EventSpecCreated, trigger.EventPayload{ID: ts.ID(), Name: ts.Name()})
		trigger.Notify(trigger.SpecUpdate)
		trigger.Redirect(w, routepath.PageSpecs)
		response.NoContent(w, r)
		return
	}

	if err = a.specSVC.Upsert(r.Context(), ts); err != nil {
		a.logger.Error().Err(err).Str("spec", id).Msg("spec update failed")
		response.Unavailable(w, r, mode)
		return
	}
	a.logger.Info().Str("spec", id).Msg("spec updated")
	trigger.Record(trigger.EventSpecUpdated, trigger.EventPayload{ID: id, Name: ts.Name()})
	trigger.Set(w, trigger.SpecUpdate)
	response.NoContent(w, r)
}

func (a *API) specDelete(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	err := a.specSVC.Delete(r.Context(), id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		a.logger.Error().Err(err).Str("spec", id).Msg("spec delete failed")
		response.Unavailable(w, r, mode)
		return
	}
	a.logger.Info().Str("spec", id).Msg("spec deleted")
	trigger.Notify(trigger.SpecUpdate)
	trigger.Redirect(w, routepath.PageSpecs)
	response.NoContent(w, r)
}

func (a *API) specDeploy(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	if err := a.specSVC.Deploy(r.Context(), id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		a.logger.Error().Err(err).Str("spec", id).Msg("spec deploy failed")
		response.Unavailable(w, r, mode)
		return
	}

	rollouts, err := a.specSVC.RolloutsBySpec(r.Context(), id, inmemory.NewRolloutFilter().BySpecID(id))
	if err != nil {
		a.logger.Warn().Err(err).Str("spec", id).Msg("spec deployed but rollout query failed")
	} else {
		a.logger.Info().Str("spec", id).Int("rollouts", len(rollouts)).Msg("spec deployed")
		if a.logger.GetLevel() <= zerolog.DebugLevel {
			for _, ss := range rollouts {
				a.logger.Debug().
					Str("rollout_id", ss.ID()).
					Str("agent_id", ss.AgentID()).
					Str("status", ss.Status().String()).
					Int("desired", ss.DesiredVersion()).
					Int("actual", ss.ActualVersion()).
					Msg("deploy rollout")
			}
		}
	}

	var specName string
	if ts, err := a.specSVC.Get(r.Context(), id); err == nil {
		specName = ts.Name()
	}
	trigger.Record(trigger.EventSpecDeployed, trigger.EventPayload{ID: id, Name: specName})
	trigger.Set(w, trigger.SpecUpdate)
	response.NoContent(w, r)
}

func (a *API) specRollouts(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	states, err := a.specSVC.RolloutsBySpec(r.Context(), id, inmemory.NewRolloutFilter().BySpecID(id))
	if err != nil {
		a.logger.Error().Err(err).Str("spec", id).Msg("spec rollouts failed")
		response.Unavailable(w, r, mode)
		return
	}

	a.logger.Info().Str("spec", id).Int("count", len(states)).Msg("spec rollouts loaded")
	if a.logger.GetLevel() <= zerolog.DebugLevel {
		for _, ss := range states {
			a.logger.Debug().
				Str("rollout_id", ss.ID()).
				Str("agent_id", ss.AgentID()).
				Str("status", ss.Status().String()).
				Int("desired", ss.DesiredVersion()).
				Int("actual", ss.ActualVersion()).
				Int("attempts", ss.Attempts()).
				Str("error", ss.Error()).
				Msg("rollout entry")
		}
	}

	items := mapSlice(states, apimapv1.RolloutEntry)
	response.OK(w, r, mode, &responder.View{
		Data:      items,
		Component: contentSpec.Rollouts(items),
	})
}

// identity returns the authenticated identity from the request context.
func (a *API) identity(r *http.Request) *identity.Identity {
	id, _ := transportctx.Identity(r.Context())
	return id
}

// actor returns the display name of the authenticated user from the request context.
func (a *API) actor(r *http.Request) string {
	if id := a.identity(r); id != nil {
		return id.Name
	}
	return "unknown"
}

// decodeJSON reads and decodes a JSON request body into v.
func decodeJSON[T any](r *http.Request) (T, error) {
	var v T
	err := json.NewDecoder(r.Body).Decode(&v)
	return v, err
}

// queryInt reads a query parameter as int, returning fallback if absent or invalid.
func queryInt(r *http.Request, key string, fallback int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	return fallback
}

// mapSlice converts a slice of pointers using fn, skipping nils.
func mapSlice[T any, U any](src []*T, fn func(*T) U) []U {
	out := make([]U, 0, len(src))
	for _, v := range src {
		if v == nil {
			continue
		}
		out = append(out, fn(v))
	}
	return out
}

// guard wraps an http.HandlerFunc with a permission check and serves it.
func guard(w http.ResponseWriter, r *http.Request, perm kind.Permission, fn http.HandlerFunc) {
	middleware.RequirePermission(perm)(fn).ServeHTTP(w, r)
}

// endpoint pairs an HTTP method with its permission and handler.
type endpoint struct {
	method string
	perm   kind.Permission
	fn     func(http.ResponseWriter, *http.Request, httpctx.RenderMode)
}

// resource handles a collection-style handler: mode, optional path check, method dispatch with guard.
func (a *API) resource(w http.ResponseWriter, r *http.Request, path string, routes ...endpoint) {
	mode := httpctx.ModeFromRequest(r)
	if path != "" && r.URL.Path != path {
		response.NotFound(w, r, mode)
		return
	}
	for _, rt := range routes {
		if rt.method == r.Method {
			guard(w, r, rt.perm, func(w http.ResponseWriter, r *http.Request) {
				rt.fn(w, r, mode)
			})
			return
		}
	}
	response.NotAllowed(w, r, mode)
}

// subroute pairs an action name, HTTP method, permission and handler for entity subrouting.
// action "" matches the root (e.g. /api/v1/users/{id}).
type subroute struct {
	action string
	method string
	perm   kind.Permission
	fn     func(http.ResponseWriter, *http.Request, httpctx.RenderMode, string)
}

// handles /{prefix}/{id}[/{action}] dispatching: parses id and optional action,
// then matches against the route table. Wrong method → 405, unknown action → 404.
func (a *API) router(w http.ResponseWriter, r *http.Request, prefix string, routes ...subroute) {
	mode := httpctx.ModeFromRequest(r)
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if rest == "" {
		response.NotFound(w, r, mode)
		return
	}

	id, tail, _ := strings.Cut(rest, "/")
	if id == "" {
		response.NotFound(w, r, mode)
		return
	}

	action := ""
	if tail != "" {
		var extra string
		action, extra, _ = strings.Cut(tail, "/")
		if extra != "" {
			response.NotFound(w, r, mode)
			return
		}
	}

	found := false
	for _, rt := range routes {
		if rt.action != action {
			continue
		}
		found = true
		if rt.method == r.Method {
			guard(w, r, rt.perm, func(w http.ResponseWriter, r *http.Request) {
				rt.fn(w, r, mode, id)
			})
			return
		}
	}
	if found {
		response.NotAllowed(w, r, mode)
	} else {
		response.NotFound(w, r, mode)
	}
}
