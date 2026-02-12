package handlers

import (
	"errors"
	"net/http"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/svc"
	"github.com/soltiHQ/control-plane/internal/backend"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/cookie"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transportctx"
	content "github.com/soltiHQ/control-plane/ui/templates/content/user"
	my "github.com/soltiHQ/control-plane/ui/templates/page"
)

// UI serves browser-facing HTML endpoints (and HTMX blocks).
type UI struct {
	logger zerolog.Logger
	auth   *svc.Auth

	loginUC  *backend.Login
	agentsUC *backend.Agents
	usersUC  *backend.Users
}

func NewUI(logger zerolog.Logger, auth *svc.Auth, loginUC *backend.Login, agentsUC *backend.Agents, usersUC *backend.Users) *UI {
	return &UI{
		logger:   logger,
		auth:     auth,
		loginUC:  loginUC,
		agentsUC: agentsUC,
		usersUC:  usersUC,
	}
}

// Main renders GET /
func (x *UI) Main(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path != "/" {
		response.NotFound(w, r, response.RenderPage)
		return
	}
	id, _ := transportctx.Identity(r.Context())
	nav := backend.BuildNav(id)

	response.OK(w, r, response.RenderPage, &responder.View{
		Component: my.Main(nav),
	})
}

// Login handles GET/POST /login
func (x *UI) Login(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		redirect := r.URL.Query().Get("redirect")
		if redirect == "" {
			redirect = "/"
		}
		errMsg := r.URL.Query().Get("error")

		response.OK(w, r, response.RenderPage, &responder.View{
			Component: my.Login(redirect, errMsg),
		})
		return

	case http.MethodPost:
		// ok
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		response.BadRequest(w, r, response.RenderPage)
		return
	}

	subject := r.FormValue("subject")
	password := r.FormValue("password")
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = "/"
	}

	key := subject // rate-limit key (пока так; позже subject+ip)

	_, access, refresh, sessionID, err := x.loginUC.Do(r.Context(), subject, password, key)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			response.AuthRateLimit(w, r, response.RenderPage)

		case errors.Is(err, auth.ErrInvalidCredentials),
			errors.Is(err, auth.ErrInvalidRequest):
			http.Redirect(
				w, r,
				"/login",
				http.StatusFound,
			)

		default:
			x.logger.Warn().Err(err).Str("subject", subject).Msg("login failed")
			http.Redirect(
				w, r,
				"/login",
				http.StatusFound,
			)
		}
		return
	}

	cookie.SetAuth(w, r, access, refresh, sessionID)
	http.Redirect(w, r, redirect, http.StatusFound)
}

// Logout handles POST/GET /logout
func (x *UI) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/logout" {
		response.NotFound(w, r, response.RenderPage)
		return
	}

	if c, err := cookie.GetSessionID(r); err == nil && c != nil && c.Value != "" {
		_ = x.auth.Session.Revoke(r.Context(), c.Value)
	}

	cookie.DeleteAuth(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

//// Agents renders GET /agents (HTMX block).
//func (x *UI) Agents(w http.ResponseWriter, r *http.Request) {
//	if r.Method != http.MethodGet {
//		w.WriteHeader(http.StatusMethodNotAllowed)
//		return
//	}
//	if r.URL.Path != "/agents" {
//		response.NotFound(w, r, response.RenderPage)
//		return
//	}
//
//	// пока без пагинации/фильтров
//	res, err := x.agentsUC.List(r.Context(), 100, "")
//	if err != nil {
//		x.logger.Error().Err(err).Msg("list agents failed")
//		response.Unavailable(w, r, response.RenderPage)
//		return
//	}
//
//	response.OK(w, r, response.RenderBlock, &responder.View{
//		Component: blocks.Agents(res.Items),
//	})
//}

// Users renders GET /users
func (x *UI) Users(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path != "/users" {
		response.NotFound(w, r, response.RenderPage)
		return
	}
	id, _ := transportctx.Identity(r.Context())
	nav := backend.BuildNav(id)

	response.OK(w, r, response.RenderPage, &responder.View{
		Component: my.Users(nav),
	})
}

// UsersList renders GET /user-list
// UsersList renders GET /users/list (block)
func (x *UI) UsersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/users/list" {
		response.NotFound(w, r, response.RenderPage)
		return
	}

	cursor := r.URL.Query().Get("cursor")
	q := r.URL.Query().Get("q")

	var filter storage.UserFilter
	if q != "" {
		filter = inmemory.NewUserFilter().Query(q)
	}

	res, err := x.usersUC.List(r.Context(), 5, cursor, filter)
	if err != nil {
		x.logger.Error().Err(err).Msg("list users failed")
		response.Unavailable(w, r, response.RenderBlock)
		return
	}

	response.OK(w, r, response.RenderBlock, &responder.View{
		Component: content.List(res.Items, res.NextCursor, q),
	})
}

// UsersListRows renders GET /users/list/rows (block append + oob footer)
func (x *UI) UsersListRows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/users/list/rows" {
		response.NotFound(w, r, response.RenderPage)
		return
	}

	cursor := r.URL.Query().Get("cursor")
	q := r.URL.Query().Get("q")

	var filter storage.UserFilter
	if q != "" {
		filter = inmemory.NewUserFilter().Query(q)
	}

	res, err := x.usersUC.List(r.Context(), 5, cursor, filter)
	if err != nil {
		x.logger.Error().Err(err).Msg("list users failed")
		response.Unavailable(w, r, response.RenderBlock)
		return
	}

	response.OK(w, r, response.RenderBlock, &responder.View{
		Component: content.RowsResponse(res.Items, res.NextCursor, q),
	})
}

func (x *UI) UsersForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/users/new" {
		response.NotFound(w, r, response.RenderPage)
		return
	}

	id, _ := transportctx.Identity(r.Context())
	nav := backend.BuildNav(id)

	response.OK(w, r, response.RenderPage, &responder.View{
		Component: my.UserCreate(nav, "", "", "", "", "", "", "", "", ""),
	})
}

// UsersCreate renders GET /users/create and handles POST /users/create
func (x *UI) UsersCreate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/users/create" {
		response.NotFound(w, r, response.RenderPage)
		return
	}

	idn, _ := transportctx.Identity(r.Context())
	nav := backend.BuildNav(idn)

	// (по-хорошему это middleware RequirePermission(kind.UsersAdd), но можно и тут)
	if idn == nil || !idn.HasPermission(kind.UsersAdd) {
		response.Forbidden(w, r, response.RenderPage)
		return
	}

	switch r.Method {
	case http.MethodGet:
		response.OK(w, r, response.RenderPage, &responder.View{
			Component: my.UserCreate(
				nav,
				"", "", "", "",
				"", "", "", "",
				"",
			),
		})
		return

	case http.MethodPost:
		// ok
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		response.BadRequest(w, r, response.RenderPage)
		return
	}

	formID := r.FormValue("id")
	formSubject := r.FormValue("subject")
	formName := r.FormValue("name")
	formEmail := r.FormValue("email")

	var (
		idErr      string
		subjectErr string
		nameErr    string
		emailErr   string
		formErr    string
	)

	if formID == "" {
		idErr = "required"
	}
	if formSubject == "" {
		subjectErr = "required"
	}

	if idErr != "" || subjectErr != "" || nameErr != "" || emailErr != "" {
		response.OK(w, r, response.RenderPage, &responder.View{
			Component: my.UserCreate(
				nav,
				formID, formSubject, formName, formEmail,
				idErr, subjectErr, nameErr, emailErr,
				"",
			),
		})
		return
	}

	_, err := x.usersUC.Create(r.Context(), formID, formSubject, formName, formEmail)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrAlreadyExists):
			formErr = "user with same id or subject already exists"
		case errors.Is(err, storage.ErrInvalidArgument):
			formErr = "invalid input"
		default:
			x.logger.Error().Err(err).Msg("create user failed")
			formErr = "internal error"
		}

		response.OK(w, r, response.RenderPage, &responder.View{
			Component: my.UserCreate(
				nav,
				formID, formSubject, formName, formEmail,
				idErr, subjectErr, nameErr, emailErr,
				formErr,
			),
		})
		return
	}

	http.Redirect(w, r, "/users", http.StatusFound)
}
