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

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
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

// loginFixture is the UI login handler over an in-memory store that holds
// the user alice. The login limiter has the default configuration: five
// failures block.
type loginFixture struct {
	ui    *UI
	hub   *event.Hub
	store *inmemory.Store
	logs  *bytes.Buffer // the UI handler's log
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
	cfg.JWTSecret = strings.Repeat("k", auth.MinJWTSecretLen)
	authModel := kit.New(store, cfg)
	f.ui = NewUI(zerolog.New(f.logs), access.New(authModel, store, logger), spec.New(store, logger), f.hub, nil, func(*http.Request) bool { return false })
	return f
}

// setPassword gives alice the password.
func (f *loginFixture) setPassword(t *testing.T, password string) {
	t.Helper()
	if err := credential.New(f.store, zerolog.Nop()).SetPassword(context.Background(), credential.SetPasswordRequest{UserID: "user-alice", Password: password, Cost: 4}); err != nil {
		t.Fatal(err)
	}
}

// post submits the login form from the TCP peer remoteAddr.
func (f *loginFixture) post(form url.Values, remoteAddr, userAgent string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, routepath.PageLogin, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("User-Agent", userAgent)
	r.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	f.ui.Login(w, r)
	return w
}

func (f *loginFixture) login(subject, password, remoteAddr string) *httptest.ResponseRecorder {
	return f.post(url.Values{"subject": {subject}, "password": {password}}, remoteAddr, "test-browser")
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
