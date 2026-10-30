package handler

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/segmentio/ksuid"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/session"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	wire "github.com/soltiHQ/control-plane/domain/wire"
	contentUser "github.com/soltiHQ/control-plane/ui/templates/content/user"
)

// userStatusMode selects enable/disable branch in userSetStatus.
type userStatusMode uint8

const (
	userDisable userStatusMode = iota
	userActive
)

// Users handles /api/v1/users.
//
// Supported:
//   - GET  /api/v1/users
//   - POST /api/v1/users
func (a *API) Users(w http.ResponseWriter, r *http.Request) {
	route.Resource(w, r, routepath.ApiUsers,
		route.Endpoint{Method: http.MethodGet, Perm: enum.UsersGet, Fn: a.userList},
		route.Endpoint{Method: http.MethodPost, Perm: enum.UsersAdd, Fn: func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode) {
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
	route.Router(w, r, routepath.ApiUser,
		route.Subroute{Action: "", Method: http.MethodGet, Perm: enum.UsersGet, Fn: a.usersDetails},
		route.Subroute{Action: "", Method: http.MethodPut, Perm: enum.UsersEdit, Fn: func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode, id string) {
			a.userUpsert(w, r, m, id, modeUpdate)
		}},
		route.Subroute{Action: "", Method: http.MethodDelete, Perm: enum.UsersDelete, Fn: a.userDelete},
		route.Subroute{Action: "sessions", Method: http.MethodGet, Perm: enum.UsersGet, Fn: a.usersSessions},
		route.Subroute{Action: "disable", Method: http.MethodPost, Perm: enum.UsersEdit, Fn: func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode, id string) {
			a.userSetStatus(w, r, m, id, userDisable)
		}},
		route.Subroute{Action: "enable", Method: http.MethodPost, Perm: enum.UsersEdit, Fn: func(w http.ResponseWriter, r *http.Request, m httpctx.RenderMode, id string) {
			a.userSetStatus(w, r, m, id, userActive)
		}},
		route.Subroute{Action: "password", Method: http.MethodPost, Perm: enum.UsersEdit, Fn: a.userSetPassword},
	)
}

// SessionsRouter handles /api/v1/session/{id} action subroutes.
//
// Supported:
//   - POST /api/v1/session/{sessionID}/revoke
func (a *API) SessionsRouter(w http.ResponseWriter, r *http.Request) {
	route.Router(w, r, routepath.ApiSession,
		route.Subroute{Action: "revoke", Method: http.MethodPost, Perm: enum.UsersEdit, Fn: a.userRevokeSession},
	)
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

	items := mapSlice(res.Items, wire.UserToREST)
	response.OK(w, r, mode, &responder.View{
		Data: restv1.ListUsersResponse{
			Items:      items,
			NextCursor: res.NextCursor,
		},
		Component: contentUser.List(res.Items, res.NextCursor, q),
	})
}

func (a *API) usersDetails(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	u, err := a.userSVC.Get(r.Context(), id)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			a.logger.Error().Err(err).Str("user_id", id).Msg("user get failed")
		}
		response.FromError(w, r, mode, err)
		return
	}

	manageable, err := a.userSVC.CanManage(r.Context(), a.identity(r), u)
	if err != nil {
		a.logger.Error().Err(err).Str("user_id", id).Msg("user permissions failed")
		response.Unavailable(w, r, mode)
		return
	}

	apiUser := wire.UserToREST(u)
	response.OK(w, r, mode, &responder.View{
		Data:      apiUser,
		Component: contentUser.Detail(apiUser, policy.BuildUserDetail(a.identity(r), id, manageable)),
	})
}

func (a *API) usersSessions(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	res, err := a.sessionSVC.ListByUser(r.Context(), session.ListByUserQuery{UserID: id})
	if err != nil {
		a.logger.Error().Err(err).Str("user_id", id).Msg("user sessions list failed")
		response.Unavailable(w, r, mode)
		return
	}
	target, err := a.userSVC.Get(r.Context(), id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		a.logger.Error().Err(err).Str("user_id", id).Msg("user get failed")
		response.Unavailable(w, r, mode)
		return
	}
	if err != nil {
		target = nil // sessions left by a deleted user have no owner to protect
	}
	manageable, err := a.userSVC.CanManage(r.Context(), a.identity(r), target)
	if err != nil {
		a.logger.Error().Err(err).Str("user_id", id).Msg("user permissions failed")
		response.Unavailable(w, r, mode)
		return
	}

	items := mapSlice(res.Items, wire.SessionToREST)
	slices.SortFunc(items, func(a, b restv1.Session) int {
		pa := enum.DeriveSessionStatus(a.Revoked, a.ExpiresAt).Priority()
		pb := enum.DeriveSessionStatus(b.Revoked, b.ExpiresAt).Priority()
		if pa != pb {
			return cmp.Compare(pa, pb)
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	response.OK(w, r, mode, &responder.View{
		Data:      restv1.ListSessionsResponse{Items: items},
		Component: contentUser.Sessions(items, policy.BuildUserDetail(a.identity(r), id, manageable)),
	})
}

func (a *API) userUpsert(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string, action upsertMode) {
	in, err := decodeJSON[restv1.User](r)
	if err != nil {
		badBody(w, r, mode, err)
		return
	}

	actor := a.identity(r)
	if action == modeCreate && (in.RoleIDs != nil || in.Permissions != nil) {
		if actor == nil || !actor.HasPermission(enum.UsersEdit) {
			response.Forbidden(w, r, mode)
			return
		}
	}
	for _, p := range in.Permissions {
		if p != "" && !enum.Permission(p).Known() {
			response.BadRequestMsg(w, r, mode, "Unknown permission.")
			return
		}
	}
	var (
		u   *model.User
		uid = id
	)

	switch action {
	case modeCreate:
		u, err = model.NewUser(ksuid.New().String(), in.Subject)
		if err != nil {
			response.BadRequestMsg(w, r, mode, "subject is required")
			return
		}
		u.Enable()
		if in.Name != "" {
			u.SetName(in.Name)
		}
		if in.Email != "" {
			u.SetEmail(in.Email)
		}
		if in.RoleIDs != nil {
			u.SetRoleIDs(in.RoleIDs)
		}
		if in.Permissions != nil {
			u.SetPermissions(in.Permissions)
		}
		uid = u.ID()
		err = a.userSVC.Create(r.Context(), actor, u)
	case modeUpdate:
		if id == "" || (in.ID != "" && in.ID != id) {
			response.BadRequest(w, r, mode)
			return
		}
		if actor != nil && actor.UserID == id && (in.RoleIDs != nil || in.Permissions != nil) {
			response.BadRequestMsg(w, r, mode, "You cannot change your own roles or permissions.")
			return
		}
		// The service reads the user inside its transaction and applies the
		// patch there, so a change to a field the patch does not set is not
		// overwritten. A role_ids or permissions list the body sets still
		// replaces the stored one.
		u, err = a.userSVC.Update(r.Context(), actor, id, userPatch(in))
	default:
		response.BadRequest(w, r, mode)
		return
	}
	if err != nil {
		if writeUserAccessError(w, r, mode, err) {
			return
		}
		switch {
		case errors.Is(err, domain.ErrInvalidSubject):
			response.BadRequestMsg(w, r, mode, "subject is required")
		case errors.Is(err, domain.ErrInvalidEmail):
			response.BadRequestMsg(w, r, mode, "invalid email address")
		case errors.Is(err, storage.ErrAlreadyExists):
			response.Conflict(w, r, mode, "user with this subject already exists")
		case errors.Is(err, storage.ErrNotFound):
			// The user does not exist or was deleted meanwhile, or a role it names does not exist.
			response.NotFound(w, r, mode)
		default:
			a.logger.Error().Err(err).Str("user_id", uid).Msg("user upsert failed")
			response.Unavailable(w, r, mode)
		}
		return
	}

	by := a.actor(r)
	if action == modeCreate {
		a.logger.Info().Str("user_id", u.ID()).Str("subject", u.Subject()).Msg("user created")
		a.hub.Record(event.UserCreated, event.Payload{ID: u.ID(), Name: u.Name(), By: by})
		a.hub.Notify(event.RefreshUsers)
		htmx.Redirect(w, routepath.PageUsers)
		response.NoContent(w, r)
		return
	}
	a.logger.Info().Str("user_id", id).Msg("user updated")
	a.hub.Record(event.UserUpdated, event.Payload{
		ID: u.ID(), Name: u.Name(), By: by,
	})
	htmx.Trigger(w, event.RefreshUsers)
	a.hub.Notify(event.RefreshUsers)
	response.NoContent(w, r)
}

// userPatch is the change a PUT body asks for: an empty string or a missing
// list leaves the stored value as it is.
func userPatch(in restv1.User) user.UpdatePatch {
	var p user.UpdatePatch
	if in.Name != "" {
		p.Name = &in.Name
	}
	if in.Email != "" {
		p.Email = &in.Email
	}
	if in.Subject != "" {
		p.Subject = &in.Subject
	}
	if in.RoleIDs != nil {
		p.RoleIDs = &in.RoleIDs
	}
	if in.Permissions != nil {
		p.Permissions = &in.Permissions
	}
	return p
}

func (a *API) userDelete(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	actor := a.identity(r)
	if actor != nil && actor.UserID == id {
		response.BadRequestMsg(w, r, mode, "You cannot delete your own account.")
		return
	}
	var name string
	if u, err := a.userSVC.Get(r.Context(), id); err == nil {
		name = u.Name()
	}

	err := a.userSVC.Delete(r.Context(), actor, id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		if writeUserAccessError(w, r, mode, err) {
			return
		}
		a.logger.Error().Err(err).Str("user_id", id).Msg("user delete failed")
		response.Unavailable(w, r, mode)
		return
	}
	a.logger.Info().Str("user_id", id).Msg("user deleted")
	a.hub.Record(event.UserDeleted, event.Payload{
		ID: id, Name: name, By: a.actor(r),
	})
	a.hub.Notify(event.RefreshUsers)
	htmx.Redirect(w, routepath.PageUsers)
	response.NoContent(w, r)
}

func (a *API) userSetStatus(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, userID string, status userStatusMode) {
	actor := a.identity(r)
	if status == userDisable && actor != nil && actor.UserID == userID {
		response.BadRequestMsg(w, r, mode, "You cannot disable your own account.")
		return
	}
	u, err := a.userSVC.SetDisabled(r.Context(), actor, userID, status == userDisable)
	if err != nil {
		if writeUserAccessError(w, r, mode, err) {
			return
		}
		if errors.Is(err, storage.ErrNotFound) {
			response.NotFound(w, r, mode)
			return
		}
		a.logger.Error().Err(err).Str("user_id", userID).Msg("user status update failed")
		response.Unavailable(w, r, mode)
		return
	}
	a.logger.Info().Str("user_id", userID).Msg("user status changed")
	detail := "inactive"
	if status == userActive {
		detail = "active"
	}
	a.hub.Record(event.UserStatusChanged, event.Payload{
		ID: u.ID(), Name: u.Name(), By: a.actor(r), Detail: detail,
	})
	htmx.Trigger(w, event.RefreshUsers)
	a.hub.Notify(event.RefreshUsers)
	response.NoContent(w, r)
}

func (a *API) userSetPassword(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, userID string) {
	in, err := decodeJSON[restv1.SetPasswordRequest](r)
	if err != nil {
		badBody(w, r, mode, err)
		return
	}
	if in.Password == "" {
		response.BadRequest(w, r, mode)
		return
	}

	err = a.credentialSVC.SetPasswordAs(r.Context(), a.identity(r), credential.SetPasswordRequest{
		UserID:   userID,
		Password: in.Password,
	})
	if err != nil {
		if writeUserAccessError(w, r, mode, err) {
			return
		}
		switch {
		case errors.Is(err, storage.ErrNotFound):
			response.NotFound(w, r, mode)
		case errors.Is(err, auth.ErrWeakPassword):
			response.BadRequestMsg(w, r, mode, "Password must be 12-72 characters.")
		case errors.Is(err, auth.ErrUserDisabled):
			response.BadRequestMsg(w, r, mode, "user is disabled")
		default:
			a.logger.Error().Err(err).Str("user_id", userID).Msg("password change failed")
			response.Unavailable(w, r, mode)
		}
		return
	}

	a.logger.Info().Str("user_id", userID).Msg("user password changed")
	var userName string
	if u, err := a.userSVC.Get(r.Context(), userID); err == nil {
		userName = u.Name()
	}
	a.hub.Record(event.UserPasswordChanged, event.Payload{
		ID: userID, Name: userName, By: a.actor(r),
	})
	htmx.Trigger(w, event.RefreshUsers)
	a.hub.Notify(event.RefreshUsers)
	response.NoContent(w, r)
}

func (a *API) userRevokeSession(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, id string) {
	err := a.sessionSVC.Revoke(
		r.Context(),
		a.identity(r),
		session.RevokeRequest{ID: id, At: time.Now()},
	)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		if writeUserAccessError(w, r, mode, err) {
			return
		}
		a.logger.Error().Err(err).Str("session_id", id).Msg("session revoke failed")
		response.Unavailable(w, r, mode)
		return
	}

	a.logger.Info().Str("session_id", id).Msg("session revoked")
	htmx.Trigger(w, event.RefreshSessions)
	a.hub.Notify(event.RefreshSessions)
	response.NoContent(w, r)
}

// writeUserAccessError answers the user-management ceiling and last-admin
// errors and reports whether err was one of them.
func writeUserAccessError(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, err error) bool {
	switch {
	case errors.Is(err, user.ErrPermissionCeiling):
		response.Forbidden(w, r, mode)
	case errors.Is(err, user.ErrLastAdmin):
		response.Conflict(w, r, mode, "At least one enabled user must keep all permissions.")
	default:
		return false
	}
	return true
}
