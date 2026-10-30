package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

const (
	// browserOrigin serves the UI pages in these tests.
	browserOrigin = "http://podium.test"
	// browserPassword is the password of every fixture user.
	browserPassword = "browser-test-password"

	formType = "application/x-www-form-urlencoded"
	jsonType = "application/json"
)

// syncBuffer is a log sink that is safe to read while the handler writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// browserFixture is the main listener over an in-memory store holding alice,
// who has every permission, and bob, who may only read agents.
type browserFixture struct {
	handler http.Handler
	store   *inmemory.Store
	logs    *syncBuffer
}

// newBrowserFixture builds the main listener from cfg. wrap, when set, wraps
// the store the listener uses; the fixture reads and seeds the inner one.
func newBrowserFixture(t *testing.T, cfg config.Config, wrap func(storage.Storage) storage.Storage) *browserFixture {
	t.Helper()
	ctx := context.Background()
	store := inmemory.New()
	creds := credential.New(store, zerolog.Nop())
	all := make([]string, 0, len(enum.All))
	for _, p := range enum.All {
		all = append(all, string(p))
	}
	for _, u := range []struct {
		id, subject string
		permissions []string
	}{
		{"user-alice", "alice", all},
		{"user-bob", "bob", []string{string(enum.AgentsGet)}},
	} {
		user, err := model.NewUser(u.id, u.subject)
		if err != nil {
			t.Fatal(err)
		}
		user.SetPermissions(u.permissions)
		if err := store.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		if err := creds.SetPassword(ctx, credential.SetPasswordRequest{UserID: u.id, Password: browserPassword, Cost: 4}); err != nil {
			t.Fatal(err)
		}
	}
	var st storage.Storage = store
	if wrap != nil {
		st = wrap(store)
	}
	logs := &syncBuffer{}
	return &browserFixture{handler: mainHandlerOver(t, cfg, st, zerolog.New(logs)), store: store, logs: logs}
}

func (f *browserFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

// browserRequest is a request a UI page sends: same origin, with cookies.
func browserRequest(method, target, contentType, body string, cookies []*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, browserOrigin+target, strings.NewReader(body))
	r.Header.Set("Origin", browserOrigin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

// loginForm is the login form as submitted for subject.
func loginForm(subject string) string {
	return url.Values{"subject": {subject}, "password": {browserPassword}}.Encode()
}

// login signs subject in through the login form and returns its auth cookies.
func (f *browserFixture) login(t *testing.T, subject string) []*http.Cookie {
	t.Helper()
	w := f.serve(browserRequest(http.MethodPost, "/login", formType, loginForm(subject), nil))
	if w.Code != http.StatusFound {
		t.Fatalf("login as %s: HTTP %d, want 302", subject, w.Code)
	}
	return w.Result().Cookies()
}

// session returns the stored session the cookies name.
func (f *browserFixture) session(t *testing.T, cookies []*http.Cookie) *model.Session {
	t.Helper()
	s, err := f.store.GetSession(context.Background(), cookieValue(cookies, httpcookie.NameSessionID))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func cookieValue(cookies []*http.Cookie, name string) string {
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// without returns cookies minus the one called name.
func without(cookies []*http.Cookie, name string) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range cookies {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}

// clearsAuthCookies reports whether w expires all three auth cookies.
func clearsAuthCookies(w *httptest.ResponseRecorder) bool {
	cleared := 0
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 && c.Value == "" {
			cleared++
		}
	}
	return cleared == 3
}

// A browser sends Sec-Fetch-Site, and Origin with every unsafe method. A
// mutation from another site, or one whose Origin does not match the host, is
// refused before any handler runs.
func TestCrossOriginMutationRejected(t *testing.T) {
	f := newBrowserFixture(t, config.Default(), nil)
	alice := f.login(t, "alice")
	for _, tc := range []struct {
		name                              string
		site                              string // Sec-Fetch-Site; empty for a browser that does not send it
		method, target, contentType, body string
		cookies                           []*http.Cookie
	}{
		{"cross-site login form", "cross-site", http.MethodPost, "/login", formType, loginForm("alice"), nil},
		{"cross-site logout", "cross-site", http.MethodPost, "/logout", formType, "", alice},
		{"cross-site user create", "cross-site", http.MethodPost, "/api/v1/users", jsonType, `{"subject":"mallory"}`, alice},
		{"same-site user delete", "same-site", http.MethodDelete, "/api/v1/users/user-bob", "", "", alice},
		{"browser without Sec-Fetch-Site", "", http.MethodPost, "/api/v1/users", jsonType, `{"subject":"mallory"}`, alice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := browserRequest(tc.method, tc.target, tc.contentType, tc.body, tc.cookies)
			r.Header.Set("Origin", "https://other.example.com")
			r.Header.Del("Sec-Fetch-Site")
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			w := f.serve(r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("HTTP %d %s, want 403", w.Code, w.Body.String())
			}
			if cookies := w.Result().Cookies(); len(cookies) != 0 {
				t.Fatalf("refused request set cookies %v", cookies)
			}
			if !strings.HasPrefix(tc.target, "/api/") {
				// Rendered by the responder Negotiate picked for a page.
				if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
					t.Fatalf("refused page request answered as %q, want HTML", got)
				}
				return
			}
			var body struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Message != "Cross-origin request blocked." {
				t.Fatalf("refusal %q (%v), want the message %q", w.Body.String(), err, "Cross-origin request blocked.")
			}
		})
	}
	// Refusals happen before any route, so CrossOrigin itself logs them.
	refusals := 0
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var entry struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Level == "warn" && entry.Message == "cross-origin request blocked" {
			refusals++
		}
	}
	if refusals != 5 {
		t.Errorf("%d refusals logged at WARN, want 5:\n%s", refusals, f.logs.String())
	}
	ctx := context.Background()
	if _, err := f.store.GetUserBySubject(ctx, "mallory"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a refused request created a user: %v", err)
	}
	if _, err := f.store.GetUser(ctx, "user-bob"); err != nil {
		t.Errorf("a refused request deleted bob: %v", err)
	}
	if f.session(t, alice).Revoked() {
		t.Error("a refused logout ended alice's session")
	}
}

// Every mutation the UI makes still works under the cross-origin check and
// the JSON Content-Type rule, sent the way the UI sends it: the login and
// logout forms, the JSON fetches of the modals and the spec editor, and the
// HTMX forms (form-encoded, with HX-Request and the Accept header that
// control-plane.js adds to every HTMX mutation).
func TestSameOriginMutationsAllowed(t *testing.T) {
	ctx := context.Background()
	f := newBrowserFixture(t, config.Default(), nil)
	bobSession, err := model.NewSession("bob-session", "user-bob", "cred-user-bob", enum.Password, make([]byte, 32), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateSession(ctx, bobSession); err != nil {
		t.Fatal(err)
	}
	agentHTTP := httptest.NewServer(http.NotFoundHandler())
	defer agentHTTP.Close()
	ag, err := model.NewAgentFrom(model.AgentParams{ID: "agent-1", Name: "agent-1", Endpoint: agentHTTP.URL, EndpointType: 2, APIVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"spec-1", "spec-2"} {
		ts, err := model.NewSpec(id, "Spec "+id, "slot-"+id)
		if err != nil {
			t.Fatal(err)
		}
		ts.SetKindType(enum.TaskKindSubprocess)
		ts.SetKindConfig(map[string]any{"mode": map[string]any{"command": map[string]any{"command": "echo"}}})
		if err := f.store.UpsertSpec(ctx, ts); err != nil {
			t.Fatal(err)
		}
	}

	alice := f.login(t, "alice")
	for _, tc := range []struct {
		name, method, target string
		kind                 string // "json" (fetch), "htmx" or "form" (a plain HTML form)
		body                 string
		want                 int
	}{
		{"user create", http.MethodPost, "/api/v1/users", "json", `{"subject":"carol"}`, http.StatusNoContent},
		{"user edit", http.MethodPut, "/api/v1/users/user-bob", "json", `{"name":"Bob"}`, http.StatusNoContent},
		{"user disable", http.MethodPost, "/api/v1/users/user-bob/disable", "htmx", "", http.StatusNoContent},
		{"user enable", http.MethodPost, "/api/v1/users/user-bob/enable", "htmx", "", http.StatusNoContent},
		{"session revoke", http.MethodPost, "/api/v1/session/bob-session/revoke", "htmx", "", http.StatusNoContent},
		{"user password", http.MethodPost, "/api/v1/users/user-bob/password", "json", `{"password":"bob-new-password"}`, http.StatusNoContent},
		{"user delete", http.MethodDelete, "/api/v1/users/user-bob", "htmx", "", http.StatusNoContent},
		{"agent labels", http.MethodPut, "/api/v1/agents/agent-1/labels", "json", `{"env":"prod"}`, http.StatusNoContent},
		// The request reaches the agent, which knows no such task.
		{"task cancel", http.MethodPost, "/api/v1/agents/agent-1/tasks/task-1/cancel", "htmx", "", http.StatusNotFound},
		{"spec create", http.MethodPost, "/api/v1/specs", "json", `{"name":"New spec","slot":"new-slot","kind_type":"subprocess","kind_config":{"mode":{"command":{"command":"echo"}}}}`, http.StatusNoContent},
		{"spec save", http.MethodPut, "/api/v1/specs/spec-1", "json", `{"version":1,"name":"Renamed"}`, http.StatusNoContent},
		{"spec deploy", http.MethodPost, "/api/v1/specs/spec-1/deploy", "htmx", "", http.StatusNoContent},
		{"spec delete", http.MethodDelete, "/api/v1/specs/spec-1", "htmx", "", http.StatusNoContent},
		{"spec force delete", http.MethodDelete, "/api/v1/specs/spec-2/force", "htmx", "", http.StatusNoContent},
		{"issue dismiss", http.MethodDelete, "/api/v1/dashboard/issues?kind=agent_inactive&entity=agent-1", "htmx", "", http.StatusNoContent},
		{"logout", http.MethodPost, "/logout", "form", "", http.StatusSeeOther},
	} {
		contentType := formType
		if tc.kind == "json" {
			contentType = jsonType
		}
		r := browserRequest(tc.method, tc.target, contentType, tc.body, alice)
		if tc.kind != "form" {
			r.Header.Set("HX-Request", "true")
			r.Header.Set("Accept", "application/json")
		}
		if w := f.serve(r); w.Code != tc.want {
			t.Errorf("%s: HTTP %d %s, want %d", tc.name, w.Code, w.Body.String(), tc.want)
		}
	}

	t.Run("origin listed in cors.allow_origins", func(t *testing.T) {
		cfg := config.Default()
		cfg.CORS.AllowOrigins = []string{"https://app.example.com"}
		f := newBrowserFixture(t, cfg, nil)
		r := browserRequest(http.MethodPost, "/api/v1/users", jsonType, `{"subject":"dave"}`, f.login(t, "alice"))
		r.Header.Set("Origin", "https://app.example.com")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		if w := f.serve(r); w.Code != http.StatusNoContent {
			t.Fatalf("HTTP %d %s, want 204", w.Code, w.Body.String())
		}
	})

	t.Run("API client without browser headers", func(t *testing.T) {
		f := newBrowserFixture(t, config.Default(), nil)
		token := cookieValue(f.login(t, "alice"), httpcookie.NameAccessToken)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"subject":"erin"}`))
		r.Header.Set("Content-Type", jsonType)
		r.Header.Set("Authorization", "Bearer "+token)
		if w := f.serve(r); w.Code != http.StatusNoContent {
			t.Fatalf("HTTP %d %s, want 204", w.Code, w.Body.String())
		}
	})
}

// Every response that sets or clears the auth cookies, or sets the device
// cookie, marks them Secure when the client used HTTPS, also behind a trusted
// TLS-terminating proxy, or always with auth.cookie_secure "always".
func TestAuthCookiesSecureBehindTrustedProxy(t *testing.T) {
	for _, tc := range []struct {
		name, cookieSecure, peer, proto string
		want                            bool
	}{
		{"https through a trusted proxy", "auto", "10.0.0.1:443", "https", true},
		{"http through a trusted proxy", "auto", "10.0.0.1:443", "http", false},
		{"https header from an untrusted peer", "auto", "203.0.113.9:40000", "https", false},
		{"plain http", "auto", "203.0.113.9:40000", "", false},
		{"always", "always", "203.0.113.9:40000", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.TrustedProxies = []string{"10.0.0.1"}
			cfg.Auth.CookieSecure = tc.cookieSecure
			f := newBrowserFixture(t, cfg, nil)
			serve := func(step string, r *http.Request, wantStatus, wantCookies int) []*http.Cookie {
				t.Helper()
				r.RemoteAddr = tc.peer
				if tc.proto != "" {
					r.Header.Set("X-Forwarded-Proto", tc.proto)
				}
				w := f.serve(r)
				if w.Code != wantStatus {
					t.Fatalf("%s: HTTP %d, want %d", step, w.Code, wantStatus)
				}
				cookies := w.Result().Cookies()
				if len(cookies) != wantCookies {
					t.Fatalf("%s: %d cookies, want %d", step, len(cookies), wantCookies)
				}
				for _, c := range cookies {
					if c.Secure != tc.want {
						t.Errorf("%s: cookie %s Secure = %v, want %v", step, c.Name, c.Secure, tc.want)
					}
				}
				return cookies
			}

			// Login sets the device cookie too.
			login := serve("login", browserRequest(http.MethodPost, "/login", formType, loginForm("alice"), nil), http.StatusFound, 4)
			if cookieValue(login, httpcookie.NameDeviceToken) == "" {
				t.Fatal("login set no device cookie")
			}
			serve("refresh", browserRequest(http.MethodGet, "/api/v1/users", "", "", without(login, httpcookie.NameAccessToken)), http.StatusOK, 3)
			serve("logout", browserRequest(http.MethodPost, "/logout", formType, "", login), http.StatusSeeOther, 3)
			serve("revoked session", browserRequest(http.MethodGet, "/api/v1/users", "", "", login), http.StatusUnauthorized, 3)
		})
	}
}

// Logout only answers POST, which the cross-origin check keeps same-origin.
// It ends the caller's own session, whatever the session_id cookie says.
func TestLogoutRequiresPostAndRevokesAuthenticatedSession(t *testing.T) {
	f := newBrowserFixture(t, config.Default(), nil)
	alice := f.login(t, "alice")
	bob := f.login(t, "bob")

	if w := f.serve(browserRequest(http.MethodGet, "/logout", "", "", alice)); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /logout: HTTP %d, want 405", w.Code)
	}
	if f.session(t, alice).Revoked() {
		t.Fatal("GET /logout ended the session")
	}

	// alice's tokens with bob's session_id cookie.
	mixed := append(without(alice, httpcookie.NameSessionID), &http.Cookie{Name: httpcookie.NameSessionID, Value: cookieValue(bob, httpcookie.NameSessionID)})
	w := f.serve(browserRequest(http.MethodPost, "/logout", formType, "", mixed))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("POST /logout: HTTP %d to %q, want 303 to /login", w.Code, w.Header().Get("Location"))
	}
	if !clearsAuthCookies(w) {
		t.Fatalf("logout did not clear the auth cookies: %v", w.Result().Cookies())
	}
	if !f.session(t, alice).Revoked() {
		t.Error("logout left the caller's session alive")
	}
	if f.session(t, bob).Revoked() {
		t.Error("logout revoked the session named by the session_id cookie")
	}

	// Without valid credentials, logout only clears the cookies.
	w = f.serve(browserRequest(http.MethodPost, "/logout", formType, "", []*http.Cookie{{Name: httpcookie.NameSessionID, Value: cookieValue(bob, httpcookie.NameSessionID)}}))
	if w.Code != http.StatusSeeOther || !clearsAuthCookies(w) {
		t.Fatalf("unauthenticated logout: HTTP %d, cookies %v; want 303 clearing the cookies", w.Code, w.Result().Cookies())
	}
	if f.session(t, bob).Revoked() {
		t.Error("unauthenticated logout revoked a session")
	}
}

// failingRevoke is a store whose session revokes fail.
type failingRevoke struct{ storage.Storage }

func (failingRevoke) RevokeSession(context.Context, string, time.Time) error {
	return storage.ErrUnavailable
}

// When the revoke fails the session lives on, so logout must not pretend it
// worked: 503, an ERROR log line, and the cookies stay.
func TestLogoutRevokeFailureKeepsSession(t *testing.T) {
	f := newBrowserFixture(t, config.Default(), func(s storage.Storage) storage.Storage { return failingRevoke{s} })
	alice := f.login(t, "alice")

	w := f.serve(browserRequest(http.MethodPost, "/logout", formType, "", alice))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /logout: HTTP %d, want 503", w.Code)
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("failed logout touched the cookies: %v", cookies)
	}
	if f.session(t, alice).Revoked() {
		t.Fatal("session revoked although the store failed")
	}
	logged := false
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var entry struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Level == "error" && strings.Contains(entry.Message, "logout") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no ERROR log line for the failed logout:\n%s", f.logs.String())
	}
	if w := f.serve(browserRequest(http.MethodGet, "/api/v1/users", "", "", alice)); w.Code != http.StatusOK {
		t.Fatalf("after the failed logout: HTTP %d, want 200 (still signed in)", w.Code)
	}
}

// sessionOutage is a store whose session reads fail while down is set.
type sessionOutage struct {
	storage.Storage
	down atomic.Bool
}

func (s *sessionOutage) GetSession(ctx context.Context, id string) (*model.Session, error) {
	if s.down.Load() {
		return nil, storage.ErrUnavailable
	}
	return s.Storage.GetSession(ctx, id)
}

// expiredAccessToken is an access token for the session the cookies name that
// expired a minute ago, beyond the verifier's clock-skew allowance.
func expiredAccessToken(t *testing.T, cookies []*http.Cookie) string {
	t.Helper()
	cfg := config.Default().Auth.WithDefaults()
	now := time.Now()
	raw, err := jwt.NewHSIssuer([]byte(mainHandlerSecret), token.RealClock()).Issue(context.Background(), &identity.Identity{
		IssuedAt: now.Add(-20 * time.Minute), NotBefore: now.Add(-20 * time.Minute), ExpiresAt: now.Add(-time.Minute),
		Issuer: cfg.Issuer, Audience: []string{cfg.Audience},
		Subject: "alice", UserID: "user-alice", TokenID: "expired-token", SessionID: cookieValue(cookies, httpcookie.NameSessionID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// While the store cannot tell whether a session is valid, logout must not
// pretend it worked: 503, no cookie written, and the session lives on. That
// holds for a valid access token, whose session check fails, and for an
// expired one, whose refresh fails. Other requests get 503 too, not 401, and
// every such 503 is logged at ERROR with its method and path.
func TestLogoutDuringStoreOutageKeepsSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expired bool
	}{{"access token", false}, {"expired access token", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var outage *sessionOutage
			f := newBrowserFixture(t, config.Default(), func(s storage.Storage) storage.Storage {
				outage = &sessionOutage{Storage: s}
				return outage
			})
			alice := f.login(t, "alice")
			cookies := alice
			if tc.expired {
				cookies = append(without(alice, httpcookie.NameAccessToken), &http.Cookie{Name: httpcookie.NameAccessToken, Value: expiredAccessToken(t, alice)})
			}

			outage.down.Store(true)
			for _, r := range []*http.Request{
				browserRequest(http.MethodPost, "/logout", formType, "", cookies),
				browserRequest(http.MethodGet, "/api/v1/users", "", "", cookies),
			} {
				w := f.serve(r)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s %s during the outage: HTTP %d, want 503", r.Method, r.URL.Path, w.Code)
				}
				if written := w.Result().Cookies(); len(written) != 0 {
					t.Fatalf("%s %s during the outage wrote cookies %v", r.Method, r.URL.Path, written)
				}
				logged := false
				for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
					var entry struct {
						Level  string `json:"level"`
						Method string `json:"method"`
						Path   string `json:"path"`
					}
					if json.Unmarshal([]byte(line), &entry) == nil && entry.Level == "error" && entry.Method == r.Method && entry.Path == r.URL.Path {
						logged = true
					}
				}
				if !logged {
					t.Fatalf("%s %s during the outage: no ERROR log line with its method and path:\n%s", r.Method, r.URL.Path, f.logs.String())
				}
			}
			outage.down.Store(false)

			if f.session(t, alice).Revoked() {
				t.Fatal("session revoked during the outage")
			}
			if w := f.serve(browserRequest(http.MethodGet, "/api/v1/users", "", "", alice)); w.Code != http.StatusOK {
				t.Fatalf("after the outage: HTTP %d, want 200 (still signed in)", w.Code)
			}
		})
	}
}

// A refresh for a user that was deleted answers 401, as for any refresh token
// that is no good, not 503 as for a store that cannot tell.
func TestRefreshForDeletedUserIsUnauthorized(t *testing.T) {
	f := newBrowserFixture(t, config.Default(), nil)
	alice := f.login(t, "alice")
	if err := f.store.DeleteUser(context.Background(), "user-alice"); err != nil {
		t.Fatal(err)
	}
	cookies := append(without(alice, httpcookie.NameAccessToken), &http.Cookie{Name: httpcookie.NameAccessToken, Value: expiredAccessToken(t, alice)})
	if w := f.serve(browserRequest(http.MethodGet, "/api/v1/users", "", "", cookies)); w.Code != http.StatusUnauthorized {
		t.Fatalf("refresh for a deleted user: HTTP %d, want 401", w.Code)
	}
}
