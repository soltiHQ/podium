package handlers

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/ui/pages"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/svc"
	"github.com/soltiHQ/control-plane/internal/backend"
	"github.com/soltiHQ/control-plane/internal/transport/http/cookie"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/ui/blocks"
	my "github.com/soltiHQ/control-plane/ui/templates/page"
)

// UI serves browser-facing HTML endpoints (and HTMX blocks).
type UI struct {
	logger zerolog.Logger
	auth   *svc.Auth

	loginUC  *backend.Login
	agentsUC *backend.Agents
}

func NewUI(logger zerolog.Logger, auth *svc.Auth, loginUC *backend.Login, agentsUC *backend.Agents) *UI {
	return &UI{
		logger:   logger,
		auth:     auth,
		loginUC:  loginUC,
		agentsUC: agentsUC,
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

	response.OK(w, r, response.RenderPage, &responder.View{
		Component: pages.Main(),
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
				"/login?error=Invalid+credentials&redirect="+url.QueryEscape(redirect),
				http.StatusFound,
			)

		default:
			x.logger.Warn().Err(err).Str("subject", subject).Msg("login failed")
			http.Redirect(
				w, r,
				"/login?error=Login+failed&redirect="+url.QueryEscape(redirect),
				http.StatusFound,
			)
		}
		return
	}

	cookie.SetAuth(w, r, access, refresh, sessionID)
	http.Redirect(w, r, redirect, http.StatusFound)
}

// Agents renders GET /agents (HTMX block).
func (x *UI) Agents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/agents" {
		response.NotFound(w, r, response.RenderPage)
		return
	}

	// пока без пагинации/фильтров
	res, err := x.agentsUC.List(r.Context(), 100, "")
	if err != nil {
		x.logger.Error().Err(err).Msg("list agents failed")
		response.Unavailable(w, r, response.RenderPage)
		return
	}

	response.OK(w, r, response.RenderBlock, &responder.View{
		Component: blocks.Agents(res.Items),
	})
}

// Test renders GET /test
func (x *UI) Test(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path != "/test" {
		response.NotFound(w, r, response.RenderPage)
		return
	}

	response.OK(w, r, response.RenderPage, &responder.View{
		Component: my.Main(),
	})
}
