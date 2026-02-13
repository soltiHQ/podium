package handler

import (
	"net/http"
	"strconv"

	"github.com/rs/zerolog"
	v1 "github.com/soltiHQ/control-plane/api/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/transport/http/apimap"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	contentUser "github.com/soltiHQ/control-plane/ui/templates/content/user"
)

// API handlers.
type API struct {
	logger    zerolog.Logger
	accessSVC *access.Service
	userSVC   *user.Service
}

// NewAPI creates a new API handler.
func NewAPI(logger zerolog.Logger, accessSVC *access.Service, userSVC *user.Service) *API {
	if accessSVC == nil {
		panic("handler.API: accessSVC is nil")
	}
	if userSVC == nil {
		panic("handler.API: userSVC is nil")
	}
	return &API{
		logger:    logger.With().Str("handler", "api").Logger(),
		accessSVC: accessSVC,
		userSVC:   userSVC,
	}
}

// Routes registers API routes.
func (a *API) Routes(mux *http.ServeMux, auth route.BaseMW, perm route.PermMW, common ...route.BaseMW) {
	route.HandleFunc(
		mux,
		"/api/v1/users",
		a.UsersList,
		append(common, auth, perm(kind.UsersGet))...,
	)
}

// UsersList handles GET /api/v1/users
//
// Query params:
//   - limit: int (optional)
//   - cursor: string (optional)
//   - q: string (optional, currently only used for HTML template; filtering should be done via storage filter factory)
func (a *API) UsersList(w http.ResponseWriter, r *http.Request) {
	mode := response.ModeFromRequest(r)

	if r.Method != http.MethodGet {
		response.NotAllowed(w, r, mode)
		return
	}
	if r.URL.Path != "/api/v1/users" {
		response.NotFound(w, r, mode)
		return
	}

	var (
		limit  int
		cursor = r.URL.Query().Get("cursor")
		q      = r.URL.Query().Get("q")
	)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}

	res, err := a.userSVC.List(r.Context(), user.ListQuery{
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		a.logger.Error().Err(err).Msg("api: list users failed")
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
