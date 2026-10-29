package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/devicecookie"
	"github.com/soltiHQ/control-plane/internal/auth/httpcookie"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

const (
	loginSubject  = "alice"
	loginPassword = "correct-horse-battery-staple"
	loginIP       = "203.0.113.9:40000"
)

// loginSecret is the JWT secret of the fixture's auth stack, from which the
// device cookie key is derived.
var loginSecret = strings.Repeat("k", auth.MinJWTSecretLen)

// loginFixture is the UI login handler over an in-memory store that holds
// the user alice. The login limiter has the default configuration: five
// failures block.
type loginFixture struct {
	ui     *UI
	hub    *event.Hub
	store  *inmemory.Store
	logs   *bytes.Buffer // the UI handler's log
	secure bool          // the cookie Secure decision
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	return newLoginFixtureWith(t, auth.Config{})
}

// newLoginFixtureWith is newLoginFixture with cfg for the auth stack; the JWT
// secret is filled in.
func newLoginFixtureWith(t *testing.T, cfg auth.Config) *loginFixture {
	t.Helper()
	ctx := context.Background()
	logger := zerolog.Nop()
	store := inmemory.New()
	u, err := model.NewUser("user-alice", loginSubject)
	if err != nil {
		t.Fatal(err)
	}
	u.SetPermissions([]string{string(enum.AgentsGet)})
	if err := store.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	f := &loginFixture{store: store, logs: &bytes.Buffer{}}
	f.setPassword(t, loginPassword)
	f.hub = event.NewHub(logger)
	t.Cleanup(f.hub.Close)
	cfg.JWTSecret = loginSecret
	authModel := kit.New(store, cfg)
	f.ui = NewUI(zerolog.New(f.logs), access.New(authModel, store, logger), spec.New(store, logger), f.hub, nil, func(*http.Request) bool { return f.secure })
	return f
}

// setPassword gives alice the password.
func (f *loginFixture) setPassword(t *testing.T, password string) {
	t.Helper()
	if err := credential.New(f.store, zerolog.Nop()).SetPassword(context.Background(), credential.SetPasswordRequest{UserID: "user-alice", Password: password, Cost: 4}); err != nil {
		t.Fatal(err)
	}
}

// post submits the login form from the TCP peer remoteAddr, with cookies.
func (f *loginFixture) post(form url.Values, remoteAddr, userAgent string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, routepath.PageLogin, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("User-Agent", userAgent)
	r.RemoteAddr = remoteAddr
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	f.ui.Login(w, r)
	return w
}

func (f *loginFixture) login(subject, password, remoteAddr string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return f.post(url.Values{"subject": {subject}, "password": {password}}, remoteAddr, "test-browser", cookies...)
}

// blockAccount blocks subject with five failed logins, each from its own
// client IP, none of them loginIP.
func (f *loginFixture) blockAccount(t *testing.T, subject string) {
	t.Helper()
	for i := range 5 {
		if w := f.login(subject, "wrong-password", fmt.Sprintf("198.51.100.%d:40000", i+1)); w.Code != http.StatusSeeOther {
			t.Fatalf("failure %d on %s: HTTP %d, want 303", i+1, subject, w.Code)
		}
	}
}

// deviceCookie returns the device cookie that w sets.
func deviceCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == httpcookie.NameDeviceToken {
			return c
		}
	}
	t.Fatalf("HTTP %d sets no device cookie", w.Code)
	return nil
}

func (f *loginFixture) rateLimitedEvents() []event.Record {
	var out []event.Record
	for _, rec := range f.hub.RecentEvents(100) {
		if rec.Kind == event.RateLimited {
			out = append(out, rec)
		}
	}
	return out
}

func TestLoginLimiterNotBypassedByUserAgent(t *testing.T) {
	f := newLoginFixture(t)
	for i := range 5 {
		form := url.Values{"subject": {loginSubject}, "password": {"wrong-password"}}
		if w := f.post(form, loginIP, fmt.Sprintf("browser/%d", i)); w.Code != http.StatusSeeOther {
			t.Fatalf("failure %d: HTTP %d, want 303", i+1, w.Code)
		}
	}
	form := url.Values{"subject": {loginSubject}, "password": {loginPassword}}
	if w := f.post(form, loginIP, "browser/fresh"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt with a new User-Agent: HTTP %d, want 429", w.Code)
	}
}

func TestRateLimitedEventRecordedOncePerBlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		attempt func(i int) (subject, remoteAddr string)
	}{
		{"account and IP block together", func(int) (string, string) { return loginSubject, loginIP }},
		{"IP blocks", func(i int) (string, string) { return fmt.Sprintf("user-%d", i), loginIP }},
		{"account blocks", func(i int) (string, string) { return loginSubject, fmt.Sprintf("198.51.100.%d:40000", i+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			for i := range 5 {
				subject, remoteAddr := tc.attempt(i)
				if w := f.login(subject, "wrong-password", remoteAddr); w.Code != http.StatusSeeOther {
					t.Fatalf("failure %d: HTTP %d, want 303", i+1, w.Code)
				}
				want := 0
				if i == 4 {
					want = 1
				}
				if got := len(f.rateLimitedEvents()); got != want {
					t.Fatalf("after failure %d: %d rate_limited events, want %d", i+1, got, want)
				}
			}
			for i := 5; i < 8; i++ {
				subject, remoteAddr := tc.attempt(i)
				if w := f.login(subject, "wrong-password", remoteAddr); w.Code != http.StatusTooManyRequests {
					t.Fatalf("attempt %d: HTTP %d, want 429", i+1, w.Code)
				}
			}
			events := f.rateLimitedEvents()
			if len(events) != 1 {
				t.Fatalf("%d rate_limited events after refused attempts, want 1", len(events))
			}
			subject, _ := tc.attempt(4)
			if p := events[0].Payload; p.ID != subject || p.Name != subject || p.By != "auth" {
				t.Fatalf("rate_limited payload %+v, want the subject %q by auth", p, subject)
			}
		})
	}
}

func TestSuccessfulLoginResetsOnlyTheAccount(t *testing.T) {
	f := newLoginFixture(t)
	for i := range 4 {
		if w := f.login(loginSubject, "wrong-password", loginIP); w.Code != http.StatusSeeOther {
			t.Fatalf("failure %d: HTTP %d, want 303", i+1, w.Code)
		}
	}
	if w := f.login(loginSubject, loginPassword, loginIP); w.Code != http.StatusFound {
		t.Fatalf("correct password: HTTP %d, want 302", w.Code)
	}

	// The account starts over: four more failures do not block it.
	const otherIP = "198.51.100.7:40000"
	for i := range 4 {
		if w := f.login(loginSubject, "wrong-password", otherIP); w.Code != http.StatusSeeOther {
			t.Fatalf("failure %d after the reset: HTTP %d, want 303", i+1, w.Code)
		}
	}
	if w := f.login(loginSubject, loginPassword, otherIP); w.Code != http.StatusFound {
		t.Fatalf("correct password after four new failures: HTTP %d, want 302", w.Code)
	}

	// The first IP keeps its four failures: one more blocks it.
	if w := f.login("bob", "wrong-password", loginIP); w.Code != http.StatusSeeOther {
		t.Fatalf("fifth failure from the first IP: HTTP %d, want 303", w.Code)
	}
	if w := f.login("carol", "wrong-password", loginIP); w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt from the blocked IP: HTTP %d, want 429", w.Code)
	}
}

// The subject is normalized the way user storage normalizes it, and the log
// names the account as stored.
func TestLoginNormalizesSubject(t *testing.T) {
	f := newLoginFixture(t)
	if w := f.login(" Alice ", loginPassword, loginIP); w.Code != http.StatusFound {
		t.Fatalf(`login as " Alice ": HTTP %d, want 302`, w.Code)
	}
	var sessions []event.Record
	for _, rec := range f.hub.RecentEvents(100) {
		if rec.Kind == event.SessionCreated {
			sessions = append(sessions, rec)
		}
	}
	if len(sessions) != 1 || sessions[0].Payload.ID != "user-alice" {
		t.Fatalf("session_created events %+v, want one for user-alice", sessions)
	}
	if logs := f.logs.String(); !strings.Contains(logs, `"subject":"alice"`) || strings.Contains(logs, "Alice") {
		t.Fatalf("login log does not name the account as alice:\n%s", logs)
	}
}

// However the subject is typed, the rate_limited event names the account the
// way it is stored, so its issue groups and dismisses as one.
func TestRateLimitedEventNamesTheAccount(t *testing.T) {
	f := newLoginFixture(t)
	for i, subject := range []string{" Alice ", "ALICE", "alice ", "aLiCe", "Alice"} {
		if w := f.login(subject, "wrong-password", fmt.Sprintf("198.51.100.%d:40000", i+1)); w.Code != http.StatusSeeOther {
			t.Fatalf("failure %d as %q: HTTP %d, want 303", i+1, subject, w.Code)
		}
	}
	events := f.rateLimitedEvents()
	if len(events) != 1 || events[0].Payload.ID != "alice" || events[0].Payload.Name != "alice" {
		t.Fatalf("rate_limited events %+v, want one naming alice", events)
	}
}

// A password over the 72 bytes bcrypt reads is a wrong password even when its
// first 72 bytes are right: the login fails like any other and counts toward
// the limit.
func TestLoginPasswordOverBcryptLimitCountsAsFailure(t *testing.T) {
	f := newLoginFixtureWith(t, auth.Config{RateAttempts: 2})
	password := strings.Repeat("p", 72)
	f.setPassword(t, password)
	for i := range 2 {
		if w := f.login(loginSubject, password+"x", loginIP); w.Code != http.StatusSeeOther {
			t.Fatalf("73-byte password, attempt %d: HTTP %d, want 303", i+1, w.Code)
		}
	}
	if w := f.login(loginSubject, password, loginIP); w.Code != http.StatusTooManyRequests {
		t.Fatalf("right password after two failures: HTTP %d, want 429", w.Code)
	}
}

func TestLoginRejectsOversizedInput(t *testing.T) {
	f := newLoginFixture(t)
	if w := f.login(strings.Repeat("a", 257), "wrong-password", loginIP); w.Code != http.StatusBadRequest {
		t.Fatalf("257-byte subject: HTTP %d, want 400", w.Code)
	}
	if w := f.login(strings.Repeat("a", 256), "wrong-password", loginIP); w.Code != http.StatusSeeOther {
		t.Fatalf("256-byte subject: HTTP %d, want 303", w.Code)
	}
	if w := f.login(loginSubject, strings.Repeat("p", 16<<10), loginIP); w.Code != http.StatusBadRequest {
		t.Fatalf("body over 16 KiB: HTTP %d, want 400", w.Code)
	}
}

// Failed logins from anywhere can keep an account, even the public admin,
// blocked. They cannot keep out a browser that logged in as that account
// before: its device cookie gets it a limit of its own, while every client
// without one still finds the account blocked.
func TestLockedAccountAdmitsKnownDevice(t *testing.T) {
	f := newLoginFixture(t)
	device := deviceCookie(t, f.login(loginSubject, loginPassword, loginIP))
	f.blockAccount(t, loginSubject)

	w := f.login(loginSubject, loginPassword, loginIP, device)
	if w.Code != http.StatusFound || w.Header().Get("Location") != routepath.PageHome {
		t.Fatalf("known device with the right password: HTTP %d to %q, want 302 to %s", w.Code, w.Header().Get("Location"), routepath.PageHome)
	}
	var sessionID string
	for _, c := range w.Result().Cookies() {
		if c.Name == httpcookie.NameSessionID {
			sessionID = c.Value
		}
	}
	if _, err := f.store.GetSession(context.Background(), sessionID); err != nil {
		t.Fatalf("known device login stored no session %q: %v", sessionID, err)
	}

	// That success resets the device only: the account stays blocked.
	for _, remoteAddr := range []string{loginIP, "192.0.2.7:40000"} {
		if w := f.login(loginSubject, loginPassword, remoteAddr); w.Code != http.StatusTooManyRequests {
			t.Fatalf("client without the cookie at %s, right password: HTTP %d, want 429", remoteAddr, w.Code)
		}
	}
}

// A device cookie vouches only for the account that earned it: alice's cookie
// leaves an attempt on admin untrusted.
func TestDeviceCookieIsBoundToSubject(t *testing.T) {
	f := newLoginFixture(t)
	device := deviceCookie(t, f.login(loginSubject, loginPassword, loginIP))
	f.blockAccount(t, "admin")
	if w := f.login("admin", "admin-password", loginIP, device); w.Code != http.StatusTooManyRequests {
		t.Fatalf("admin with alice's device cookie: HTTP %d, want 429", w.Code)
	}
}

// Only a cookie that podium signed for the subject, at most 90 days ago and
// not in the future, makes an attempt trusted. Any other value leaves the
// attempt on the blocked account.
func TestForgedOrExpiredDeviceCookieIsUntrusted(t *testing.T) {
	f := newLoginFixture(t)
	f.blockAccount(t, loginSubject)
	signer := devicecookie.NewSigner([]byte(loginSecret))
	issue := func(at time.Time) string {
		t.Helper()
		value, err := signer.Issue(loginSubject, at)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	now := time.Now()
	valid := issue(now)
	parts := strings.Split(valid, ".")
	forged := "A" + parts[3][1:]
	if forged == parts[3] {
		forged = "B" + parts[3][1:]
	}
	for _, tc := range []struct{ name, value string }{
		{"bad mac", strings.Join([]string{parts[0], parts[1], parts[2], forged}, ".")},
		{"future timestamp", issue(now.Add(2 * time.Minute))},
		{"older than 90 days", issue(now.Add(-91 * 24 * time.Hour))},
		{"wrong version", "v2." + strings.Join(parts[1:], ".")},
		{"garbage", "not-a-device-cookie"},
	} {
		cookie := &http.Cookie{Name: httpcookie.NameDeviceToken, Value: tc.value}
		if w := f.login(loginSubject, loginPassword, loginIP, cookie); w.Code != http.StatusTooManyRequests {
			t.Errorf("%s: HTTP %d, want 429", tc.name, w.Code)
		}
	}

	// The cookie signed now is trusted: the values above failed on their own.
	if w := f.login(loginSubject, loginPassword, loginIP, &http.Cookie{Name: httpcookie.NameDeviceToken, Value: valid}); w.Code != http.StatusFound {
		t.Fatalf("valid device cookie: HTTP %d, want 302", w.Code)
	}
}

// A known device has a limit of its own: rate_attempts failures block that
// device, with one rate_limited event, and leave the account and client IP
// keys untouched.
func TestKnownDeviceHasItsOwnLimit(t *testing.T) {
	f := newLoginFixture(t)
	device := deviceCookie(t, f.login(loginSubject, loginPassword, loginIP))
	for i := range 5 {
		if w := f.login(loginSubject, "wrong-password", loginIP, device); w.Code != http.StatusSeeOther {
			t.Fatalf("failure %d with the device cookie: HTTP %d, want 303", i+1, w.Code)
		}
		want := 0
		if i == 4 {
			want = 1
		}
		if got := len(f.rateLimitedEvents()); got != want {
			t.Fatalf("after failure %d: %d rate_limited events, want %d", i+1, got, want)
		}
	}
	if w := f.login(loginSubject, loginPassword, loginIP, device); w.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked device with the right password: HTTP %d, want 429", w.Code)
	}
	events := f.rateLimitedEvents()
	if len(events) != 1 {
		t.Fatalf("%d rate_limited events, want 1", len(events))
	}
	if p := events[0].Payload; p.ID != loginSubject || p.Name != loginSubject || p.By != "auth" {
		t.Fatalf("rate_limited payload %+v, want the subject %q by auth", p, loginSubject)
	}

	// Neither the account nor the client IP counted those failures: after four
	// more failures without the cookie, from the same IP, a fifth attempt runs.
	for i := range 4 {
		if w := f.login(loginSubject, "wrong-password", loginIP); w.Code != http.StatusSeeOther {
			t.Fatalf("failure %d without the cookie: HTTP %d, want 303", i+1, w.Code)
		}
	}
	if w := f.login(loginSubject, loginPassword, loginIP); w.Code != http.StatusFound {
		t.Fatalf("right password without the cookie after four failures: HTTP %d, want 302", w.Code)
	}
}

// Every successful login sets a new device cookie: sent only to the login
// route, kept 90 days, out of reach of scripts and cross-site requests, and
// Secure per the shared cookie decision. A failed login sets none.
func TestLoginSetsDeviceCookie(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("secure %t", secure), func(t *testing.T) {
			f := newLoginFixture(t)
			f.secure = secure
			first := deviceCookie(t, f.login(loginSubject, loginPassword, loginIP))
			if !first.HttpOnly || first.SameSite != http.SameSiteStrictMode || first.Path != routepath.PageLogin || first.Secure != secure || first.MaxAge != 90*24*60*60 {
				t.Fatalf("device cookie %+v, want HttpOnly, SameSite=Strict, Path=%s, Secure=%t, Max-Age=7776000", first, routepath.PageLogin, secure)
			}
			second := deviceCookie(t, f.login(loginSubject, loginPassword, loginIP, first))
			if second.Value == first.Value {
				t.Fatal("a login with the device cookie kept it, want a new one")
			}
			for _, c := range f.login(loginSubject, "wrong-password", loginIP, second).Result().Cookies() {
				if c.Name == httpcookie.NameDeviceToken {
					t.Fatalf("failed login set a device cookie: %v", c)
				}
			}
		})
	}
}
