package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/ui/routepath"
	"github.com/soltiHQ/control-plane/internal/ui/trigger"

	v1 "github.com/soltiHQ/control-plane/api/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/session"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/apimap"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	contentUser "github.com/soltiHQ/control-plane/ui/templates/content/user"
)

type UserStatusMode uint8
type UserUpsertMode uint8

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
	logger     zerolog.Logger
	accessSVC  *access.Service
	userSVC    *user.Service
	sessionSVC *session.Service
}

// NewAPI creates a new API handler.
func NewAPI(
	logger zerolog.Logger,
	userSVC *user.Service,
	accessSVC *access.Service,
	sessionSVC *session.Service,
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
	return &API{
		logger:     logger.With().Str("handler", "api").Logger(),
		accessSVC:  accessSVC,
		userSVC:    userSVC,
		sessionSVC: sessionSVC,
	}
}

// Routes registers API routes.
// Auth runs at mux level once. Permissions are enforced per-method/per-subroute below.
func (a *API) Routes(mux *http.ServeMux, auth route.BaseMW, _ route.PermMW, common ...route.BaseMW) {
	route.HandleFunc(mux, routepath.ApiUsers, a.Users, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiUser, a.UsersRouter, append(common, auth)...)
	route.HandleFunc(mux, routepath.ApiSession, a.SessionsRouter, append(common, auth)...)
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

	items := make([]v1.User, 0, len(res.Items))
	for _, u := range res.Items {
		if u == nil {
			continue
		}
		items = append(items, apimap.User(u))
	}
	response.OK(w, r, mode, &responder.View{
		Data: v1.UserListResponse{
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

	apiUser := apimap.User(u)
	response.OK(w, r, mode, &responder.View{
		Data:      apiUser,
		Component: contentUser.Detail(apiUser),
	})
}

func (a *API) usersSessions(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	res, err := a.sessionSVC.ListByUser(r.Context(), session.ListByUserQuery{UserID: id})
	if err != nil {
		response.Unavailable(w, r, mode)
		return
	}

	items := make([]v1.Session, 0, len(res.Items))
	for _, s := range res.Items {
		if s == nil {
			continue
		}
		items = append(items, apimap.Session(s))
	}
	response.OK(w, r, mode, &responder.View{
		Data:      v1.SessionResponse{Items: items},
		Component: contentUser.Sessions(items),
	})
}

func (a *API) userUpsert(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string, action UserUpsertMode) {
	var (
		in v1.User
		u  *model.User
	)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	switch action {
	case UserCreate:
		if in.ID == "" || in.Subject == "" {
			response.BadRequest(w, r, mode)
			return
		}
		x, err := model.NewUser(in.ID, in.Subject)
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
	w.Header().Set(trigger.Header, trigger.UserUpdate)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) userDelete(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	err := a.userSVC.Delete(r.Context(), id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		response.Unavailable(w, r, mode)
		return
	}
	trigger.Redirect(w, routepath.PageUsers)
	w.WriteHeader(http.StatusNoContent)
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
	w.WriteHeader(http.StatusNoContent)
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
	w.WriteHeader(http.StatusNoContent)
}
