package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/segmentio/ksuid"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/ui/routepath"
	"github.com/soltiHQ/control-plane/internal/ui/trigger"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
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
	"github.com/soltiHQ/control-plane/internal/ui/policy"
	contentAgent "github.com/soltiHQ/control-plane/ui/templates/content/agent"
	contentSpec "github.com/soltiHQ/control-plane/ui/templates/content/taskspec"
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
	route.HandleFunc(mux, routepath.ApiPermissions, a.Permissions, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiRoles, a.Roles, append(common, auth)...)
}

// Users handles /api/v1/users.
//
// Supported:
//   - GET  /api/v1/users
//   - POST /api/v1/users
func (a *API) Users(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	if r.URL.Path != routepath.ApiUsers {
		response.NotFound(w, r, mode)
		return
	}

	switch r.Method {
	case http.MethodGet:
		middleware.RequirePermission(kind.UsersGet)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.userList(w, r, mode)
			}),
		).ServeHTTP(w, r)
		return
	case http.MethodPost:
		middleware.RequirePermission(kind.UsersAdd)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.userUpsert(w, r, mode, "", modeCreate)
			}),
		).ServeHTTP(w, r)
		return
	default:
		response.NotAllowed(w, r, mode)
		return
	}
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
	var (
		mode = httpctx.ModeFromRequest(r)
		rest = strings.Trim(strings.TrimPrefix(r.URL.Path, routepath.ApiUser), "/")
	)
	if rest == "" {
		response.NotFound(w, r, mode)
		return
	}

	userID, tail, _ := strings.Cut(rest, "/")
	if userID == "" {
		response.NotFound(w, r, mode)
		return
	}
	if tail == "" {
		switch r.Method {
		case http.MethodGet:
			middleware.RequirePermission(kind.UsersGet)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					a.usersDetails(w, r, mode, userID)
				}),
			).ServeHTTP(w, r)
			return
		case http.MethodPut:
			middleware.RequirePermission(kind.UsersEdit)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					a.userUpsert(w, r, mode, userID, modeUpdate)
				}),
			).ServeHTTP(w, r)
			return
		case http.MethodDelete:
			middleware.RequirePermission(kind.UsersDelete)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					a.userDelete(w, r, mode, userID)
				}),
			).ServeHTTP(w, r)
			return
		default:
			response.NotAllowed(w, r, mode)
			return
		}
	}

	action, extra, _ := strings.Cut(tail, "/")
	if extra != "" {
		response.NotFound(w, r, mode)
		return
	}
	switch action {
	case "sessions":
		if r.Method != http.MethodGet {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.UsersGet)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.usersSessions(w, r, mode, userID)
			}),
		).ServeHTTP(w, r)
		return
	case "disable":
		if r.Method != http.MethodPost {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.UsersEdit)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.userSetStatus(w, r, mode, userID, userDisable)
			}),
		).ServeHTTP(w, r)
		return
	case "enable":
		if r.Method != http.MethodPost {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.UsersEdit)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.userSetStatus(w, r, mode, userID, userActive)
			}),
		).ServeHTTP(w, r)
		return
	case "password":
		if r.Method != http.MethodPost {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.UsersEdit)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.userSetPassword(w, r, mode, userID)
			}),
		).ServeHTTP(w, r)
		return
	default:
		response.NotFound(w, r, mode)
		return
	}
}

// SessionsRouter handles /api/v1/sessions/{id} action subroutes.
//
// Supported:
//   - POST /api/v1/sessions/{sessionID}/revoke
func (a *API) SessionsRouter(w http.ResponseWriter, r *http.Request) {
	var (
		mode = httpctx.ModeFromRequest(r)
		rest = strings.Trim(strings.TrimPrefix(r.URL.Path, routepath.ApiSession), "/")
	)
	if rest == "" {
		response.NotFound(w, r, mode)
		return
	}
	sessID, action, _ := strings.Cut(rest, "/")
	if sessID == "" {
		response.NotFound(w, r, mode)
		return
	}

	if action != "revoke" {
		response.NotFound(w, r, mode)
		return
	}
	if r.Method != http.MethodPost {
		response.NotAllowed(w, r, mode)
		return
	}

	middleware.RequirePermission(kind.UsersEdit)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a.userRevokeSession(w, r, mode, sessID)
		}),
	).ServeHTTP(w, r)
}

// Agents handles /api/v1/agents.
//
// Supported:
//   - GET /api/v1/agents
func (a *API) Agents(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	if r.URL.Path != routepath.ApiAgents {
		response.NotFound(w, r, mode)
		return
	}

	switch r.Method {
	case http.MethodGet:
		middleware.RequirePermission(kind.AgentsGet)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.agentList(w, r, mode)
			}),
		).ServeHTTP(w, r)
		return
	default:
		response.NotAllowed(w, r, mode)
		return
	}
}

// AgentsRouter handles /api/v1/agents/{id} and subroutes.
//
// Supported:
//   - GET  /api/v1/agents/{id}
//   - PUT  /api/v1/agents/{id}/labels
//   - GET  /api/v1/agents/{id}/tasks
func (a *API) AgentsRouter(w http.ResponseWriter, r *http.Request) {
	var (
		mode = httpctx.ModeFromRequest(r)
		rest = strings.Trim(strings.TrimPrefix(r.URL.Path, routepath.ApiAgent), "/")
	)
	if rest == "" {
		response.NotFound(w, r, mode)
		return
	}

	agentID, tail, _ := strings.Cut(rest, "/")
	if agentID == "" {
		response.NotFound(w, r, mode)
		return
	}

	if tail == "" {
		if r.Method != http.MethodGet {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.AgentsGet)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.agentDetails(w, r, mode, agentID)
			}),
		).ServeHTTP(w, r)
		return
	}

	action, extra, _ := strings.Cut(tail, "/")
	if extra != "" {
		response.NotFound(w, r, mode)
		return
	}

	switch action {
	case "labels":
		if r.Method != http.MethodPut {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.AgentsEdit)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.agentPatchLabels(w, r, mode, agentID)
			}),
		).ServeHTTP(w, r)
		return
	case "tasks":
		if r.Method != http.MethodGet {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.AgentsGet)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.agentTasksList(w, r, mode, agentID)
			}),
		).ServeHTTP(w, r)
		return
	default:
		response.NotFound(w, r, mode)
		return
	}
}

// Permissions handles /api/v1/permissions.
//
// Supported:
//   - GET /api/v1/permissions
func (a *API) Permissions(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	switch r.Method {
	case http.MethodGet:
		middleware.RequirePermission(kind.UsersEdit)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.permissionsList(w, r, mode)
			}),
		).ServeHTTP(w, r)
		return
	default:
		response.NotAllowed(w, r, mode)
		return
	}
}

// Roles handles /api/v1/roles.
//
// Supported:
//   - GET /api/v1/roles
func (a *API) Roles(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	switch r.Method {
	case http.MethodGet:
		middleware.RequirePermission(kind.UsersEdit)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.rolesList(w, r, mode)
			}),
		).ServeHTTP(w, r)
		return
	default:
		response.NotAllowed(w, r, mode)
		return
	}
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

	items := make([]restv1.Role, 0, len(roles))
	for _, role := range roles {
		if role == nil {
			continue
		}
		items = append(items, apimapv1.Role(role))
	}
	response.OK(w, r, mode, &responder.View{
		Data: restv1.RoleListResponse{Items: items},
	})
}

func (a *API) agentList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	var (
		limit  int
		filter storage.AgentFilter

		cursor = r.URL.Query().Get("cursor")
		q      = r.URL.Query().Get("q")
	)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
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

	items := make([]restv1.Agent, 0, len(res.Items))
	for _, ag := range res.Items {
		if ag == nil {
			continue
		}
		items = append(items, apimapv1.Agent(ag))
	}
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

	identity, _ := transportctx.Identity(r.Context())
	apiAgent := apimapv1.Agent(ag)
	response.OK(w, r, mode, &responder.View{
		Data:      apiAgent,
		Component: contentAgent.Detail(apiAgent, policy.BuildAgentDetail(identity)),
	})
}

func (a *API) agentPatchLabels(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	// The Edit modal sends flat JSON { key: value, key: value }.
	// We interpret the entire body as the new labels map.
	var labels map[string]string
	if err := json.NewDecoder(r.Body).Decode(&labels); err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	_, err := a.agentSVC.PatchLabels(r.Context(), agent.PatchLabels{
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

	filter := proxy.TaskFilter{
		Slot:   r.URL.Query().Get("slot"),
		Status: r.URL.Query().Get("status"),
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			filter.Limit = n
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			filter.Offset = n
		}
	}

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

	response.OK(w, r, mode, &responder.View{
		Data:      result,
		Component: contentAgent.Tasks(agentID, result.Tasks, result.Total, filter.Slot, filter.Offset),
	})
}

func (a *API) userList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	var (
		limit  int
		filter storage.UserFilter

		cursor = r.URL.Query().Get("cursor")
		q      = r.URL.Query().Get("q")
	)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
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

	items := make([]restv1.User, 0, len(res.Items))
	for _, u := range res.Items {
		if u == nil {
			continue
		}
		items = append(items, apimapv1.User(u))
	}
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

	identity, _ := transportctx.Identity(r.Context())
	apiUser := apimapv1.User(u)
	response.OK(w, r, mode, &responder.View{
		Data:      apiUser,
		Component: contentUser.Detail(apiUser, policy.BuildUserDetail(identity, id)),
	})
}

func (a *API) usersSessions(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	res, err := a.sessionSVC.ListByUser(r.Context(), session.ListByUserQuery{UserID: id})
	if err != nil {
		a.logger.Error().Err(err).Str("user_id", id).Msg("user sessions list failed")
		response.Unavailable(w, r, mode)
		return
	}

	items := make([]restv1.Session, 0, len(res.Items))
	for _, s := range res.Items {
		if s == nil {
			continue
		}
		items = append(items, apimapv1.Session(s))
	}
	response.OK(w, r, mode, &responder.View{
		Data:      restv1.SessionResponse{Items: items},
		Component: contentUser.Sessions(items),
	})
}

func (a *API) userUpsert(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string, action upsertMode) {
	var (
		in restv1.User
		u  *model.User
	)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		response.BadRequest(w, r, mode)
		return
	}

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

	if err := a.userSVC.Upsert(r.Context(), u); err != nil {
		a.logger.Error().Err(err).Str("user_id", u.ID()).Msg("user upsert failed")
		response.Unavailable(w, r, mode)
		return
	}

	if action == modeCreate {
		a.logger.Info().Str("user_id", u.ID()).Str("subject", u.Subject()).Msg("user created")
		trigger.Redirect(w, routepath.PageUsers)
		response.NoContent(w, r)
		return
	}
	a.logger.Info().Str("user_id", id).Msg("user updated")
	trigger.Set(w, trigger.UserUpdate)
	response.NoContent(w, r)
}

func (a *API) userDelete(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	err := a.userSVC.Delete(r.Context(), id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		a.logger.Error().Err(err).Str("user_id", id).Msg("user delete failed")
		response.Unavailable(w, r, mode)
		return
	}
	a.logger.Info().Str("user_id", id).Msg("user deleted")
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
	trigger.Set(w, trigger.UserUpdate)
	response.NoContent(w, r)
}

func (a *API) userSetPassword(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, userID string) {
	var in restv1.SetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		response.BadRequest(w, r, mode)
		return
	}
	if in.Password == "" {
		response.BadRequest(w, r, mode)
		return
	}

	err := a.credentialSVC.SetPassword(r.Context(), credential.SetPasswordRequest{
		UserID:     userID,
		Password:   in.Password,
		VerifierID: "ver-" + userID,
	})
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			a.logger.Warn().Str("user_id", userID).Msg("user not found for password change")
			response.NotFound(w, r, mode)
			return
		}
		a.logger.Error().Err(err).Str("user_id", userID).Msg("user password change failed")
		response.Unavailable(w, r, mode)
		return
	}

	a.logger.Info().Str("user_id", userID).Msg("user password changed")
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
	mode := httpctx.ModeFromRequest(r)
	if r.URL.Path != routepath.ApiSpecs {
		response.NotFound(w, r, mode)
		return
	}

	switch r.Method {
	case http.MethodGet:
		middleware.RequirePermission(kind.SpecsGet)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.specList(w, r, mode)
			}),
		).ServeHTTP(w, r)
		return
	case http.MethodPost:
		middleware.RequirePermission(kind.SpecsAdd)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.specUpsert(w, r, mode, "", modeCreate)
			}),
		).ServeHTTP(w, r)
		return
	default:
		response.NotAllowed(w, r, mode)
		return
	}
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
	var (
		mode = httpctx.ModeFromRequest(r)
		rest = strings.Trim(strings.TrimPrefix(r.URL.Path, routepath.ApiSpec), "/")
	)
	if rest == "" {
		response.NotFound(w, r, mode)
		return
	}

	tsID, tail, _ := strings.Cut(rest, "/")
	if tsID == "" {
		response.NotFound(w, r, mode)
		return
	}

	if tail == "" {
		switch r.Method {
		case http.MethodGet:
			middleware.RequirePermission(kind.SpecsGet)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					a.specDetails(w, r, mode, tsID)
				}),
			).ServeHTTP(w, r)
			return
		case http.MethodPut:
			middleware.RequirePermission(kind.SpecsEdit)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					a.specUpsert(w, r, mode, tsID, modeUpdate)
				}),
			).ServeHTTP(w, r)
			return
		case http.MethodDelete:
			middleware.RequirePermission(kind.SpecsEdit)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					a.specDelete(w, r, mode, tsID)
				}),
			).ServeHTTP(w, r)
			return
		default:
			response.NotAllowed(w, r, mode)
			return
		}
	}

	action, extra, _ := strings.Cut(tail, "/")
	if extra != "" {
		response.NotFound(w, r, mode)
		return
	}

	switch action {
	case "deploy":
		if r.Method != http.MethodPost {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.SpecsDeploy)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.specDeploy(w, r, mode, tsID)
			}),
		).ServeHTTP(w, r)
		return
	case "sync":
		if r.Method != http.MethodGet {
			response.NotAllowed(w, r, mode)
			return
		}
		middleware.RequirePermission(kind.SpecsGet)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.specRollouts(w, r, mode, tsID)
			}),
		).ServeHTTP(w, r)
		return
	default:
		response.NotFound(w, r, mode)
		return
	}
}

func (a *API) specList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	var (
		limit  int
		filter storage.SpecFilter

		cursor = r.URL.Query().Get("cursor")
		q      = r.URL.Query().Get("q")
	)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
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

	items := make([]restv1.Spec, 0, len(res.Items))
	for _, ts := range res.Items {
		if ts == nil {
			continue
		}
		items = append(items, apimapv1.Spec(ts))
	}
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

	identity, _ := transportctx.Identity(r.Context())
	dto := apimapv1.RolloutSpec(ts, states)
	response.OK(w, r, mode, &responder.View{
		Data:      dto,
		Component: contentSpec.Detail(dto, policy.BuildSpecDetail(identity)),
	})
}

func (a *API) specUpsert(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string, action upsertMode) {
	var (
		in restv1.SpecCreateRequest
		ts *model.Spec
	)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		response.BadRequest(w, r, mode)
		return
	}

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
		trigger.Redirect(w, routepath.PageSpecs)
		response.NoContent(w, r)
		return
	}

	if err := a.specSVC.Upsert(r.Context(), ts); err != nil {
		a.logger.Error().Err(err).Str("spec", id).Msg("spec update failed")
		response.Unavailable(w, r, mode)
		return
	}
	a.logger.Info().Str("spec", id).Msg("spec updated")
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

	items := make([]restv1.RolloutEntry, 0, len(states))
	for _, ss := range states {
		items = append(items, apimapv1.RolloutEntry(ss))
	}
	response.OK(w, r, mode, &responder.View{
		Data:      items,
		Component: contentSpec.Rollouts(items),
	})
}
