package handlers

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/svc"
	"github.com/soltiHQ/control-plane/internal/backend"
	"github.com/soltiHQ/control-plane/internal/transport/http/cookie"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/ui/pages"
)

type UI struct {
	logger  zerolog.Logger
	auth    *svc.Auth
	backend *backend.Login
}

func NewUI(logger zerolog.Logger, auth *svc.Auth, backend *backend.Login) *UI {
	return &UI{logger: logger, auth: auth, backend: backend}
}

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

func (x *UI) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		redirect := r.URL.Query().Get("redirect")
		if redirect == "" {
			redirect = "/"
		}
		errMsg := r.URL.Query().Get("error")

		response.OK(w, r, response.RenderPage, &responder.View{
			Component: pages.Login(redirect, errMsg),
		})
		return
	}

	if r.Method != http.MethodPost {
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

	key := subject

	_, access, refresh, sessionID, err :=
		x.backend.Do(r.Context(), subject, password, key)

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
			x.logger.Warn().Err(err).Msg("login failed")
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
