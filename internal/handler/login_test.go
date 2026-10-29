package handler

import (
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
	ui  *UI
	hub *event.Hub
}

func newLoginFixture(t *testing.T) *loginFixture {
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
	if err := credential.New(store, logger).SetPassword(ctx, credential.SetPasswordRequest{UserID: u.ID(), Password: loginPassword, Cost: 4}); err != nil {
		t.Fatal(err)
	}
	hub := event.NewHub(logger)
	t.Cleanup(hub.Close)
	authModel := kit.New(store, auth.Config{JWTSecret: strings.Repeat("k", auth.MinJWTSecretLen)})
	ui := NewUI(logger, access.New(authModel, store, logger), spec.New(store, logger), hub, nil, func(*http.Request) bool { return false })
	return &loginFixture{ui: ui, hub: hub}
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

// The subject is normalized the way user storage normalizes it.
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
