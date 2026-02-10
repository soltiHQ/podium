package handlers

import (
	"net/http"
	"net/url"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/transport/http/cookie"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/ui/pages"
)

// UI handles browser-facing HTML endpoints.
type UI struct {
	html    *responder.HTMLResponder
	limiter *ratelimit.Limiter
	session *session.Service

	logger zerolog.Logger

	clock token.Clock
	err   *Fault
}

// NewUI creates a UI handler.
func NewUI(logger zerolog.Logger, session *session.Service, html *responder.HTMLResponder, limiter *ratelimit.Limiter, clk token.Clock, err *Fault) *UI {
	return &UI{
		logger:  logger,
		session: session,
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

	subject := r.URL.Query().Get("subject")
	if subject != "" {
		key := loginKey(subject, r)
		if u.limiter.Blocked(key, u.clock.Now()) {
			//u.html.Respond(w, r, http.StatusTooManyRequests, &responder.View{
			//	Component: pages.ErrorPage(
			//		http.StatusTooManyRequests,
			//		"Too many attempts",
			//		"Account temporarily locked. Please try again in 10 minutes.",
			//	),
			//})
			return
		}
	}

	errMsg := r.URL.Query().Get("error")

	u.html.Respond(w, r, http.StatusOK, &responder.View{
		Component: pages.Login(redirect, errMsg),
	})
}

// LoginSubmit handles form POST, authenticates, sets cookie, redirects.
func (u *UI) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		//u.html.Error(w, r, http.StatusBadRequest, "invalid form")
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

		response.AuthRateLimit(w, r, response.RenderPage)
		//u.err.AuthRateLimit(w, r, RenderPage)
		return
	}

	pair, id, err := u.session.Login(r.Context(), kind.Password, subject, password)
	if err != nil {
		u.limiter.RecordFailure(key, now)
		u.logger.Warn().
			Err(err).
			Str("subject", subject).
			Msg("login failed")

		http.Redirect(w, r, "/login?error=Invalid+credentials&subject="+url.QueryEscape(subject)+"&redirect="+url.QueryEscape(redirect), http.StatusFound)

		return
	}

	u.limiter.Reset(key)
	cookie.SetAuth(w, r, pair.AccessToken, pair.RefreshToken, id.SessionID)
	http.Redirect(w, r, redirect, http.StatusFound)
}
