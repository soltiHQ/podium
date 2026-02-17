package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/transport/http/cookie"
	"github.com/soltiHQ/control-plane/internal/transport/http/ratelimitkey"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transportctx"
	"github.com/soltiHQ/control-plane/internal/ui/policy"
	"github.com/soltiHQ/control-plane/internal/ui/routepath"
	pages "github.com/soltiHQ/control-plane/ui/templates/page"
	pageHome "github.com/soltiHQ/control-plane/ui/templates/page/home"
	pageSystem "github.com/soltiHQ/control-plane/ui/templates/page/system"
	pageUser "github.com/soltiHQ/control-plane/ui/templates/page/user"
)

// UI handlers
type UI struct {
	logger    zerolog.Logger
	accessSVC *access.Service
}

// NewUI creates a new UI handler.
func NewUI(logger zerolog.Logger, accessSVC *access.Service) *UI {
	if accessSVC == nil {
		panic("handler.UI: accessSVC is nil")
	}
	return &UI{
		logger:    logger.With().Str("handler", "ui").Logger(),
		accessSVC: accessSVC,
	}
}

// Routes registers UI routes.
func (u *UI) Routes(mux *http.ServeMux, auth route.BaseMW, perm route.PermMW, common ...route.BaseMW) {
	route.HandleFunc(mux, routepath.PageLogin, u.Login, common...)
	route.HandleFunc(mux, routepath.PageLogout, u.Logout, append(common, auth)...)

	route.HandleFunc(mux, routepath.PageUsers, u.Users, append(common, auth)...)
	route.HandleFunc(mux, routepath.PageUserInfo, u.UserDetail, append(common, auth, perm(kind.UsersGet))...)
	route.HandleFunc(mux, routepath.PageHome, u.Main, append(common, auth)...)
}

// Login handles GET/POST /login.
func (u *UI) Login(w http.ResponseWriter, r *http.Request) {
	mode := response.ModeFromRequest(r)

	switch r.Method {
	case http.MethodGet:
		if r.URL.Path != routepath.PageLogin {
			response.NotFound(w, r, mode)
			return
		}
		redirect := r.URL.Query().Get("redirect")
		if redirect == "" {
			redirect = routepath.PageHome
		}
		errMsg := r.URL.Query().Get("error")

		response.OK(w, r, mode, &responder.View{
			Component: pageSystem.Login(redirect, errMsg),
		})
		return
	case http.MethodPost:
		if r.URL.Path != routepath.PageLogin {
			response.NotFound(w, r, mode)
			return
		}
	default:
		response.NotAllowed(w, r, mode)
		return
	}
	if err := r.ParseForm(); err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	var (
		subject  = r.FormValue("subject")
		password = r.FormValue("password")
		redirect = r.FormValue("redirect")
		rateKey  = ratelimitkey.LoginKey(r, subject)
	)
	if redirect == "" {
		redirect = routepath.PageHome
	}
	_, res, err := u.accessSVC.Login(r.Context(), access.LoginRequest{
		Subject:  subject,
		Password: password,
		RateKey:  rateKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			response.AuthRateLimit(w, r, mode)
			return
		case errors.Is(err, auth.ErrInvalidCredentials),
			errors.Is(err, auth.ErrInvalidRequest):
			http.Redirect(w, r, routepath.PageLogin, http.StatusFound)
			return
		default:
			http.Redirect(w, r, routepath.PageLogin, http.StatusFound)
			return
		}
	}

	cookie.SetAuth(w, r, res.AccessToken, res.RefreshToken, res.SessionID)
	http.Redirect(w, r, redirect, http.StatusFound)
}

// Logout handles GET/POST /logout.
func (u *UI) Logout(w http.ResponseWriter, r *http.Request) {
	mode := response.ModeFromRequest(r)

	if r.URL.Path != routepath.PageLogout {
		response.NotFound(w, r, mode)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		response.NotAllowed(w, r, mode)
		return
	}
	if c, err := cookie.GetSessionID(r); err == nil && c != nil && c.Value != "" {
		_ = u.accessSVC.Logout(r.Context(), access.LogoutRequest{SessionID: c.Value})
	}

	cookie.DeleteAuth(w, r)
	http.Redirect(w, r, routepath.PageLogin, http.StatusFound)
}

// Main handle GET /.
func (u *UI) Main(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, http.MethodGet, routepath.PageHome, func(nav policy.Nav) templ.Component {
		return pageHome.Home(nav)
	})
}

// Users handle GET /users.
func (u *UI) Users(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, http.MethodGet, routepath.PageUsers, func(nav policy.Nav) templ.Component {
		return pages.Users(nav)
	})
}

// UserDetail handle GET /users/info/{}.
func (u *UI) UserDetail(w http.ResponseWriter, r *http.Request) {
	u.pageParam(w, r, http.MethodGet, routepath.PageUserInfo, func(nav policy.Nav, userID string) templ.Component {
		return pageUser.Detail(nav, userID)
	})
}

func (u *UI) page(w http.ResponseWriter, r *http.Request, m, p string, render func(nav policy.Nav) templ.Component) {
	mode := response.ModeFromRequest(r)
	if r.Method != m {
		response.NotAllowed(w, r, mode)
		return
	}
	if r.URL.Path != p {
		response.NotFound(w, r, mode)
		return
	}

	id, ok := transportctx.Identity(r.Context())
	if !ok {
		response.Unauthorized(w, r, mode)
		return
	}
	response.OK(w, r, mode, &responder.View{
		Component: render(policy.BuildNav(id)),
	})
}

func (u *UI) pageParam(w http.ResponseWriter, r *http.Request, m, p string, render func(nav policy.Nav, param string) templ.Component) {
	mode := response.ModeFromRequest(r)
	if r.Method != m {
		response.NotAllowed(w, r, mode)
		return
	}
	if !strings.HasPrefix(r.URL.Path, p) {
		response.NotFound(w, r, mode)
		return
	}

	param := strings.TrimPrefix(r.URL.Path, p)
	if param == "" || strings.Contains(param, "/") {
		response.NotFound(w, r, mode)
		return
	}
	id, ok := transportctx.Identity(r.Context())
	if !ok {
		response.Unauthorized(w, r, mode)
		return
	}

	response.OK(w, r, mode, &responder.View{
		Component: render(policy.BuildNav(id), param),
	})
}
