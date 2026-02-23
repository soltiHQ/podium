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
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/apimap"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transportctx"
	"github.com/soltiHQ/control-plane/internal/ui/policy"
	contentAgent "github.com/soltiHQ/control-plane/ui/templates/content/agent"
	contentUser "github.com/soltiHQ/control-plane/ui/templates/content/user"
)

type (
	UserStatusMode uint8
	UserUpsertMode uint8
)

const (
	UserDisable UserStatusMode = iota
	UserActive
)

const (
	UserCreate UserUpsertMode = iota
	UserUpdate
)

// API handlers.
type API struct {
	logger        zerolog.Logger
	accessSVC     *access.Service
	userSVC       *user.Service
	sessionSVC    *session.Service
	credentialSVC *credential.Service
	agentSVC      *agent.Service
	proxyPool     *proxy.Pool
}

// NewAPI creates a new API handler.
func NewAPI(
	logger zerolog.Logger,
	userSVC *user.Service,
	accessSVC *access.Service,
	sessionSVC *session.Service,
	credentialSVC *credential.Service,
	agentSVC *agent.Service,
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
	if proxyPool == nil {
		panic("handler.API: proxyPool is nil")
	}
	return &API{
		logger:        logger.With().Str("handler", "api").Logger(),
		accessSVC:     accessSVC,
		userSVC:       userSVC,
		sessionSVC:    sessionSVC,
		credentialSVC: credentialSVC,
		agentSVC:      agentSVC,
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
	route.HandleFunc(mux, routepath.ApiPermissions, a.Permissions, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiRoles, a.Roles, append(common, auth)...)
}

// Users handles /api/v1/users.
//
// Supported:
//   - GET  /api/v1/users
//   - POST /api/v1/users
func (a *API) Users(w http.ResponseWriter, r *http.Request) {
	mode := response.ModeFromRequest(r)
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
				a.userUpsert(w, r, mode, "", UserCreate)
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
		mode = response.ModeFromRequest(r)
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
					a.userUpsert(w, r, mode, userID, UserUpdate)
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
				a.userSetStatus(w, r, mode, userID, UserDisable)
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
				a.userSetStatus(w, r, mode, userID, UserActive)
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
		mode = response.ModeFromRequest(r)
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
	mode := response.ModeFromRequest(r)
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
		mode = response.ModeFromRequest(r)
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
	mode := response.ModeFromRequest(r)
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
	mode := response.ModeFromRequest(r)
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
		items = append(items, apimap.Permission(p))
	}
	response.OK(w, r, mode, &responder.View{
		Data: restv1.PermissionListResponse{Items: items},
	})
}

func (a *API) rolesList(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	roles, err := a.accessSVC.GetRoles(r.Context())
	if err != nil {
		response.Unavailable(w, r, mode)
		return
	}

	items := make([]restv1.Role, 0, len(roles))
	for _, role := range roles {
		if role == nil {
			continue
		}
		items = append(items, apimap.Role(role))
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
		response.Unavailable(w, r, mode)
		return
	}

	items := make([]restv1.Agent, 0, len(res.Items))
	for _, ag := range res.Items {
		if ag == nil {
			continue
		}
		items = append(items, apimap.Agent(ag))
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
		response.Unavailable(w, r, mode)
		return
	}

	identity, _ := transportctx.Identity(r.Context())
	apiAgent := apimap.Agent(ag)
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
			response.NotFound(w, r, mode)
			return
		}
		response.Unavailable(w, r, mode)
		return
	}

	w.Header().Set(trigger.Header, trigger.AgentUpdate)
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
		response.Unavailable(w, r, mode)
		return
	}

	items := make([]restv1.User, 0, len(res.Items))
	for _, u := range res.Items {
		if u == nil {
			continue
		}
		items = append(items, apimap.User(u))
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
		response.Unavailable(w, r, mode)
		return
	}

	identity, _ := transportctx.Identity(r.Context())
	apiUser := apimap.User(u)
	response.OK(w, r, mode, &responder.View{
		Data:      apiUser,
		Component: contentUser.Detail(apiUser, policy.BuildUserDetail(identity, id)),
	})
}

func (a *API) usersSessions(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	res, err := a.sessionSVC.ListByUser(r.Context(), session.ListByUserQuery{UserID: id})
	if err != nil {
		response.Unavailable(w, r, mode)
		return
	}

	items := make([]restv1.Session, 0, len(res.Items))
	for _, s := range res.Items {
		if s == nil {
			continue
		}
		items = append(items, apimap.Session(s))
	}
	response.OK(w, r, mode, &responder.View{
		Data:      restv1.SessionResponse{Items: items},
		Component: contentUser.Sessions(items),
	})
}

func (a *API) userUpsert(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string, action UserUpsertMode) {
	var (
		in restv1.User
		u  *model.User
	)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	switch action {
	case UserCreate:
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
	case UserUpdate:
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
		response.Unavailable(w, r, mode)
		return
	}

	if action == UserCreate {
		trigger.Redirect(w, routepath.PageUsers)
		response.NoContent(w, r)
		return
	}
	w.Header().Set(trigger.Header, trigger.UserUpdate)
	response.NoContent(w, r)
}

func (a *API) userDelete(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	err := a.userSVC.Delete(r.Context(), id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		response.Unavailable(w, r, mode)
		return
	}
	trigger.Redirect(w, routepath.PageUsers)
	response.NoContent(w, r)
}

func (a *API) userSetStatus(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, userID string, status UserStatusMode) {
	u, err := a.userSVC.Get(r.Context(), userID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		response.Unavailable(w, r, mode)
		return
	}

	if status == UserActive {
		u.Enable()
	} else {
		u.Disable()
	}

	if err = a.userSVC.Upsert(r.Context(), u); err != nil {
		response.Unavailable(w, r, mode)
		return
	}
	w.Header().Set(trigger.Header, trigger.UserUpdate)
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
			response.NotFound(w, r, mode)
			return
		}
		response.Unavailable(w, r, mode)
		return
	}

	w.Header().Set(trigger.Header, trigger.UserUpdate)
	response.NoContent(w, r)
}

func (a *API) userRevokeSession(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	err := a.sessionSVC.Revoke(
		r.Context(),
		session.RevokeRequest{ID: id, At: time.Now()},
	)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		response.Unavailable(w, r, mode)
		return
	}

	w.Header().Set(trigger.Header, trigger.UserSessionUpdate)
	response.NoContent(w, r)
}
