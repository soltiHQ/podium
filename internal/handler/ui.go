package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"
	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	wire "github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
	"github.com/soltiHQ/control-plane/internal/uikit/policy"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
	pageAgent "github.com/soltiHQ/control-plane/ui/templates/page/agent"
	pageHome "github.com/soltiHQ/control-plane/ui/templates/page/home"
	pageSpec "github.com/soltiHQ/control-plane/ui/templates/page/spec"
	pageSystem "github.com/soltiHQ/control-plane/ui/templates/page/system"
	pageUser "github.com/soltiHQ/control-plane/ui/templates/page/user"
)

const (
	// maxLoginBodyBytes caps the login form body.
	maxLoginBodyBytes = 16 << 10
	// maxLoginSubjectBytes caps the login subject.
	maxLoginSubjectBytes = 256
)

// UI handlers
type UI struct {
	accessSVC *access.Service
	specSVC   *spec.Service
	hub       *event.Hub
	resolver  *clientip.Resolver
	secure    func(*http.Request) bool
	logger    zerolog.Logger
}

// NewUI creates a new UI handler.
//
// resolver gives the client IP of login attempts; nil trusts no proxy. secure
// decides whether the auth and device cookies that login and logout write
// carry the Secure attribute.
func NewUI(logger zerolog.Logger, accessSVC *access.Service, specSVC *spec.Service, hub *event.Hub, resolver *clientip.Resolver, secure func(*http.Request) bool) *UI {
	if accessSVC == nil {
		panic(service.ErrNilService)
	}
	if specSVC == nil {
		panic(service.ErrNilService)
	}
	if hub == nil {
		panic(event.ErrNilHub)
	}
	if secure == nil {
		panic("handler.NewUI: nil cookie policy")
	}
	return &UI{
		logger:    logger.With().Str("handler", "ui").Logger(),
		accessSVC: accessSVC,
		specSVC:   specSVC,
		hub:       hub,
		resolver:  resolver,
		secure:    secure,
	}
}

// Routes registers UI routes. optionalAuth guards logout, which also clears
// the cookies of a caller whose session has already ended.
func (u *UI) Routes(mux *http.ServeMux, auth, optionalAuth route.BaseMW, perm route.PermMW, common ...route.BaseMW) {
	route.HandleFunc(mux, routepath.PageLogin, u.Login, common...)
	route.HandleFunc(mux, routepath.PageLogout, u.Logout, append(common, optionalAuth)...)

	route.HandleFunc(mux, routepath.PageUsers, u.Users, append(common, auth)...)
	route.HandleFunc(mux, routepath.PageUserInfo, u.UserDetail, append(common, auth, perm(enum.UsersGet))...)

	route.HandleFunc(mux, routepath.PageHistory, u.History, append(common, auth, perm(enum.AgentsGet))...)
	route.HandleFunc(mux, routepath.PageAgents, u.Agents, append(common, auth)...)
	route.HandleFunc(mux, routepath.PageAgentInfo, u.AgentDetail, append(common, auth, perm(enum.AgentsGet))...)

	route.HandleFunc(mux, routepath.PageSpecs, u.Specs, append(common, auth, perm(enum.SpecsGet))...)
	route.HandleFunc(mux, routepath.PageSpecNew, u.SpecNew, append(common, auth, perm(enum.SpecsAdd))...)
	route.HandleFunc(mux, routepath.PageSpecEdit, u.SpecEdit, append(common, auth, perm(enum.SpecsEdit))...)
	route.HandleFunc(mux, routepath.PageSpecInfo, u.SpecDetail, append(common, auth, perm(enum.SpecsGet))...)

	route.HandleFunc(mux, routepath.PageHome, u.Main, append(common, auth)...)
}

// Login handles GET/POST /login.
//
// A successful login sets the auth cookies and a new device cookie that only
// /login receives. The device cookie a login attempt sends back lets it count
// its failures apart from the account (see access.Service.Login).
func (u *UI) Login(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)

	switch r.Method {
	case http.MethodGet:
		if r.URL.Path != routepath.PageLogin {
			response.NotFound(w, r, mode)
			return
		}
		redirect := safeLoginRedirect(r.URL.Query().Get("redirect"))
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
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	if err := r.ParseForm(); err != nil {
		response.BadRequest(w, r, mode)
		return
	}

	var (
		subject  = r.FormValue("subject")
		password = r.FormValue("password")
		redirect = safeLoginRedirect(r.FormValue("redirect"))
	)
	if len(subject) > maxLoginSubjectBytes {
		response.BadRequestMsg(w, r, mode, "Username is too long.")
		return
	}
	if redirect == "" {
		redirect = routepath.PageHome
	}
	var device string
	if c, err := httpcookie.GetDevice(r); err == nil {
		device = c.Value
	}
	id, res, err := u.accessSVC.Login(r.Context(), access.LoginRequest{
		Subject:      subject,
		Password:     password,
		ClientIP:     u.resolver.IP(r),
		DeviceCookie: device,
	})
	// Logs and events name the account as stored, however the subject was typed.
	account := model.NormalizeSubject(subject)
	if errors.Is(err, auth.ErrRateLimitReached) {
		// Recorded once per block; the refused attempts that follow record nothing.
		u.hub.Record(event.RateLimited, event.Payload{
			ID: account, Name: account, By: "auth",
		})
	}
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			response.AuthRateLimit(w, r, mode)
			return
		case errors.Is(err, auth.ErrInvalidCredentials),
			errors.Is(err, auth.ErrInvalidRequest),
			errors.Is(err, auth.ErrUnauthorized):
			// A user without permissions (ErrUnauthorized) gets the answer to a
			// wrong password, which tells nothing about the password.
			if errors.Is(err, auth.ErrUnauthorized) {
				u.logger.Warn().Str("subject", account).Msg("login refused: the user has no permissions")
			}
			transportctx.SetError(r.Context(), "invalid credentials")
			http.Redirect(w, r, routepath.PageLogin+"?error=Invalid+username+or+password&redirect="+url.QueryEscape(redirect), http.StatusSeeOther)
			return
		default:
			u.logger.Error().Err(err).Str("subject", account).Msg("login error")
			http.Redirect(w, r, routepath.PageLogin+"?error=Something+went+wrong", http.StatusFound)
			return
		}
	}

	u.logger.Info().Str("subject", account).Msg("login success")
	u.hub.Record(event.SessionCreated, event.Payload{
		ID: id.UserID, Name: id.Name, By: "auth",
	})
	secure := u.secure(r)
	httpcookie.SetAuth(w, secure, res.AccessToken, res.RefreshToken, res.SessionID)
	httpcookie.SetDevice(w, secure, routepath.PageLogin, res.DeviceCookie)
	http.Redirect(w, r, redirect, http.StatusFound)
}

// Logout handles POST /logout.
//
// It revokes the session of the authenticated caller, never the one the
// session_id cookie names, then clears the auth cookies and redirects to the
// login page; a caller without valid credentials only gets the cookies
// cleared. When the revoke fails the session lives on, so the answer is 503
// and the cookies stay.
func (u *UI) Logout(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)

	if r.URL.Path != routepath.PageLogout {
		response.NotFound(w, r, mode)
		return
	}
	if r.Method != http.MethodPost {
		response.NotAllowed(w, r, mode)
		return
	}
	if id, ok := transportctx.Identity(r.Context()); ok {
		if err := u.accessSVC.Logout(r.Context(), access.LogoutRequest{SessionID: id.SessionID}); err != nil {
			u.logger.Error().Err(err).Str("user_id", id.UserID).Msg("logout failed")
			response.Unavailable(w, r, mode)
			return
		}
		u.logger.Info().Str("user_id", id.UserID).Msg("logout")
	}

	httpcookie.DeleteAuth(w, u.secure(r))
	http.Redirect(w, r, routepath.PageLogin, http.StatusSeeOther)
}

// Main handle GET /.
func (u *UI) Main(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, http.MethodGet, routepath.PageHome, func(nav policy.Nav) templ.Component { return pageHome.Home(nav) })
}

// Users handle GET /users.
func (u *UI) Users(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, http.MethodGet, routepath.PageUsers, func(nav policy.Nav) templ.Component { return pageUser.Users(nav) })
}

// Agents handle GET /agents.
func (u *UI) Agents(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, http.MethodGet, routepath.PageAgents, func(nav policy.Nav) templ.Component { return pageAgent.Agents(nav) })
}

// AgentDetail handle GET /agents/info/{}.
func (u *UI) AgentDetail(w http.ResponseWriter, r *http.Request) {
	u.pageParam(w, r, http.MethodGet, routepath.PageAgentInfo, func(nav policy.Nav, agentID string) templ.Component { return pageAgent.Detail(nav, agentID) })
}

// UserDetail handle GET /users/info/{}.
func (u *UI) UserDetail(w http.ResponseWriter, r *http.Request) {
	u.pageParam(w, r, http.MethodGet, routepath.PageUserInfo, func(nav policy.Nav, userID string) templ.Component { return pageUser.Detail(nav, userID) })
}

// Specs handle GET /specs.
func (u *UI) Specs(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, http.MethodGet, routepath.PageSpecs, func(nav policy.Nav) templ.Component { return pageSpec.Specs(nav) })
}

// SpecNew handle GET /specs/new.
func (u *UI) SpecNew(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, http.MethodGet, routepath.PageSpecNew, func(nav policy.Nav) templ.Component { return pageSpec.New(nav) })
}

// SpecDetail handle GET /specs/info/{}.
func (u *UI) SpecDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := transportctx.Identity(r.Context())
	u.pageParam(w, r, http.MethodGet, routepath.PageSpecInfo, func(nav policy.Nav, specID string) templ.Component {
		return pageSpec.Detail(nav, specID, policy.BuildSpecDetail(id))
	})
}

// SpecEdit handle GET /specs/edit/{}.
//
// The edit page cannot lazy-load the initial spec via HTMX (the builder
// form needs the values baked into Alpine's x-data on first paint to
// pre-populate inputs). We pre-fetch via specSVC, convert to the REST
// DTO, and hand it to the templ component.
func (u *UI) SpecEdit(w http.ResponseWriter, r *http.Request) {
	mode := httpctx.ModeFromRequest(r)
	if r.Method != http.MethodGet {
		response.NotAllowed(w, r, mode)
		return
	}
	specID := strings.TrimPrefix(r.URL.Path, routepath.PageSpecEdit)
	if !strings.HasPrefix(r.URL.Path, routepath.PageSpecEdit) || specID == "" || strings.Contains(specID, "/") {
		response.NotFound(w, r, mode)
		return
	}
	id, ok := transportctx.Identity(r.Context())
	if !ok {
		response.Unauthorized(w, r, mode)
		return
	}
	ts, err := u.specSVC.Get(r.Context(), specID)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			u.logger.Error().Err(err).Str("spec", specID).Msg("edit lookup failed")
		}
		response.FromError(w, r, mode, err)
		return
	}
	if ts.DeletionRequested() {
		response.Conflict(w, r, mode, "This spec is being deleted and cannot be edited.")
		return
	}
	initial := wire.SpecToREST(ts)
	response.OK(w, r, mode, &responder.View{Component: pageSpec.Edit(policy.BuildNav(id), &initial)})
}

func (u *UI) page(w http.ResponseWriter, r *http.Request, m, p string, render func(nav policy.Nav) templ.Component) {
	mode := httpctx.ModeFromRequest(r)
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
	mode := httpctx.ModeFromRequest(r)
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

// safeLoginRedirect accepts only a local absolute path. URL parsing alone
// accepts network-path references; browsers also normalize backslashes.
func safeLoginRedirect(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n") {
		return routepath.PageHome
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, "\\") {
		return routepath.PageHome
	}
	return u.RequestURI()
}
