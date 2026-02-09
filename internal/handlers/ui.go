package handlers

import (
	"net/http"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/auth/token"

	"github.com/soltiHQ/control-plane/internal/auth/auth/session"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/ui/pages"
)

// UI handles browser-facing HTML endpoints.
type UI struct {
	logger  zerolog.Logger
	session *session.Service
	store   storage.Storage
	html    *response.HTMLResponder
	limiter *ratelimit.Limiter
	clock   token.Clock
	err     *Errors
}

// NewUI creates a UI handler.
func NewUI(logger zerolog.Logger, session *session.Service, store storage.Storage, html *response.HTMLResponder, limiter *ratelimit.Limiter, clk token.Clock, err *Errors) *UI {
	return &UI{
		logger:  logger,
		session: session,
		store:   store,
		html:    html,
		limiter: limiter,
		clock:   clk,
		err:     err,
	}
}

// Routes registers UI routes on the given mux.
// These routes are public — no Auth middleware.
func (u *UI) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", u.LoginPage)
	mux.HandleFunc("POST /login", u.LoginSubmit)

}

// LoginPage renders the login form.
func (u *UI) LoginPage(w http.ResponseWriter, r *http.Request) {
	redirect := r.URL.Query().Get("redirect")
	if redirect == "" {
		redirect = "/"
	}

	errMsg := r.URL.Query().Get("error")

	u.html.Respond(w, r, http.StatusOK, &response.View{
		Component: pages.Login(redirect, errMsg),
	})
}

// LoginSubmit handles form POST, authenticates, sets cookie, redirects.
func (u *UI) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.html.Error(w, r, http.StatusBadRequest, "invalid form")
		return
	}

	subject := r.FormValue("subject")
	password := r.FormValue("password")
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = "/"
	}

	if subject == "" || password == "" {
		http.Redirect(w, r, "/login?error=Username+and+password+required&redirect="+redirect, http.StatusFound)
		return
	}

	key := loginKey(subject, r)
	now := u.clock.Now()
	if u.limiter.Blocked(key, now) {
		u.err.ManyAuthAttempts(w, r)
		return
	}

	pair, _, err := u.session.Login(r.Context(), kind.Password, subject, password)
	if err != nil {
		u.limiter.RecordFailure(key, now)
		u.logger.Warn().
			Err(err).
			Str("subject", subject).
			Msg("login failed")

		http.Redirect(w, r, "/login?error=Invalid+credentials&redirect="+redirect, http.StatusFound)
		return
	}

	u.limiter.Reset(key)

	// Set access token as HttpOnly cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     "access_token",
		Value:    pair.AccessToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	// Set refresh token as separate HttpOnly cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    pair.RefreshToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})

	http.Redirect(w, r, redirect, http.StatusFound)
}
