package handlers

import (
	"encoding/json"
	"net"
	"net/http"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
)

// Auth handles authentication endpoints.
type Auth struct {
	session *session.Service
	json    *response.JSONResponder
	limiter *ratelimit.Limiter
	clock   token.Clock
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
	SessionID    string `json:"session_id"`
}

// NewAuth creates an auth handler.
func NewAuth(session *session.Service, json *response.JSONResponder, limiter *ratelimit.Limiter, clk token.Clock) *Auth {
	return &Auth{session: session, json: json, limiter: limiter, clock: clk}
}

// Routes registers auth routes on the given mux.
// These routes are public — no Auth middleware required.
func (a *Auth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/login", a.Login)
	mux.HandleFunc("POST /v1/refresh", a.Refresh)
}

// loginRequest is the expected JSON body for login.
type loginRequest struct {
	Subject  string `json:"subject"`
	Password string `json:"password"`
}

// loginResponse is the JSON response on successful login.
type loginResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	SessionID    string   `json:"session_id"`
	ExpiresAt    int64    `json:"expires_at"`
	Subject      string   `json:"subject"`
	UserID       string   `json:"user_id"`
	Permissions  []string `json:"permissions"`
}

// Login authenticates by subject/password and returns a JWT token pair.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.json.Error(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Subject == "" || req.Password == "" {
		a.json.Error(w, r, http.StatusBadRequest, "subject and password are required")
		return
	}

	key := loginKey(req.Subject, r)
	now := a.clock.Now()

	if a.limiter.Blocked(key, now) {
		a.json.Error(w, r, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}

	pair, id, err := a.session.Login(r.Context(), kind.Password, req.Subject, req.Password)
	if err != nil {
		a.limiter.RecordFailure(key, now)
		a.json.Error(w, r, http.StatusUnauthorized, "invalid credentials")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    id.SessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	a.limiter.Reset(key)

	perms := make([]string, 0, len(id.Permissions))
	for _, p := range id.Permissions {
		perms = append(perms, string(p))
	}

	a.json.Respond(w, r, http.StatusOK, &response.View{
		Data: loginResponse{
			AccessToken:  pair.AccessToken,
			RefreshToken: pair.RefreshToken,
			ExpiresAt:    id.ExpiresAt.Unix(),
			Subject:      id.Subject,
			UserID:       id.UserID,
			Permissions:  perms,
		},
	})
}

func (a *Auth) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.json.Error(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	pair, id, err := a.session.Refresh(r.Context(), req.SessionID, req.RefreshToken)
	if err != nil {
		a.json.Error(w, r, http.StatusUnauthorized, "invalid refresh token")
		return
	}

	perms := make([]string, 0, len(id.Permissions))
	for _, p := range id.Permissions {
		perms = append(perms, string(p))
	}

	a.json.Respond(w, r, http.StatusOK, &response.View{
		Data: loginResponse{
			AccessToken:  pair.AccessToken,
			RefreshToken: pair.RefreshToken,
			ExpiresAt:    id.ExpiresAt.Unix(),
			Subject:      id.Subject,
			UserID:       id.UserID,
			Permissions:  perms,
		},
	})
}

func loginKey(subject string, r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return subject + "|" + ip
}
