package handler

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transportctx"
	"github.com/soltiHQ/control-plane/ui/templates/jsexpr"
)

// staleUserMessage is what an update answers when its If-Match names no tag of
// the stored user.
const staleUserMessage = "User changed since it was read; reload and retry."

// userETag reads the ETag that GET /api/v1/users/{id} answers a JSON client with.
func (f *apiFixture) userETag(t *testing.T, id string, viewer *identity.Identity) string {
	t.Helper()
	w := f.do(http.MethodGet, "/api/v1/users/"+id, "", viewer)
	tag := w.Header().Get("ETag")
	if w.Code != http.StatusOK || tag == "" {
		t.Fatalf("GET user %s: %d, ETag %q", id, w.Code, tag)
	}
	return tag
}

// putUser sends the update of a user with an If-Match header for each given
// value; none sends no header.
func (f *apiFixture) putUser(id, body string, actor *identity.Identity, ifMatch ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPut, "/api/v1/users/"+id, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for _, v := range ifMatch {
		r.Header.Add("If-Match", v)
	}
	r = r.WithContext(transportctx.WithIdentity(r.Context(), actor))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

// Two admins load the same user and one saves first: the other's save is based
// on a view the first save replaced, and is refused with nothing written.
func TestUserUpdateRefusesStaleETag(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	alice := f.user(t, "alice", enum.RoleAdminID)
	bob := f.user(t, "bob", enum.RoleAdminID)
	f.user(t, "target", enum.RoleUserReaderID)

	stale := f.userETag(t, "target", alice)
	if got := f.userETag(t, "target", bob); got != stale {
		t.Fatalf("two readers of one user got the ETags %s and %s", stale, got)
	}
	if w := f.putUser("target", `{"name":"Bob","role_ids":["`+enum.RoleUserEditorID+`"]}`, bob, stale); w.Code != http.StatusNoContent {
		t.Fatalf("the first save: %d %s, want 204", w.Code, w.Body.String())
	}
	current := f.userETag(t, "target", alice)
	if current == stale {
		t.Fatal("a save left the ETag as it was")
	}
	savedByBob, err := f.store.GetUser(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	events := len(f.hub.RecentEvents(100))

	// Every list that names no tag of the stored user is refused. A weak tag
	// never matches (If-Match compares strongly), and a blank header names none.
	alicesSave := `{"name":"Alice","role_ids":["` + enum.RoleUserReaderID + `"],"permissions":["agents:get"]}`
	for _, tc := range []struct {
		name    string
		ifMatch []string
	}{
		{"stale tag", []string{stale}},
		{"stale tag without quotes", []string{strings.Trim(stale, `"`)}},
		{"stale tags only", []string{stale + ", " + stale}},
		{"stale tags in two headers", []string{stale, stale}},
		{"current tag only weak", []string{"W/" + current}},
		{"blank", []string{" "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.putUser("target", alicesSave, alice, tc.ifMatch...)
			if w.Code != http.StatusPreconditionFailed {
				t.Fatalf("If-Match %q: %d %s, want 412", tc.ifMatch, w.Code, w.Body.String())
			}
			if got := responseMessage(t, w); got != staleUserMessage {
				t.Fatalf("If-Match %q: message %q, want %q", tc.ifMatch, got, staleUserMessage)
			}
			u, err := f.store.GetUser(ctx, "target")
			if err != nil {
				t.Fatal(err)
			}
			if !u.UpdatedAt().Equal(savedByBob.UpdatedAt()) || u.Name() != "Bob" ||
				!slices.Equal(u.RoleIDsAll(), []string{enum.RoleUserEditorID}) || len(u.PermissionsAll()) != 0 {
				t.Fatalf("If-Match %q: the refused save changed the user: name %q roles %v permissions %v",
					tc.ifMatch, u.Name(), u.RoleIDsAll(), u.PermissionsAll())
			}
			if got := len(f.hub.RecentEvents(100)); got != events {
				t.Fatalf("If-Match %q: the refused save recorded %d events", tc.ifMatch, got-events)
			}
			if got := f.userETag(t, "target", alice); got != current {
				t.Fatalf("If-Match %q: the refused save moved the ETag from %s to %s", tc.ifMatch, current, got)
			}
		})
	}

	// A list that names the stored user's tag applies the save, with or without
	// the quotes; "*" matches whatever is stored.
	for _, tc := range []struct {
		name    string
		ifMatch func(stale, current string) []string
	}{
		{"current tag", func(_, c string) []string { return []string{c} }},
		{"current tag without quotes", func(_, c string) []string { return []string{strings.Trim(c, `"`)} }},
		{"stale and current tags in one header", func(s, c string) []string { return []string{s + ", " + c} }},
		{"stale and current tags in two headers", func(s, c string) []string { return []string{s, c} }},
		{"star", func(_, _ string) []string { return []string{"*"} }},
	} {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			now := f.userETag(t, "target", alice)
			w := f.putUser("target", `{"name":"Alice (`+tc.name+`)"}`, alice, tc.ifMatch(stale, now)...)
			if w.Code != http.StatusNoContent {
				t.Fatalf("%d %s, want 204", w.Code, w.Body.String())
			}
			if u, err := f.store.GetUser(ctx, "target"); err != nil || u.Name() != "Alice ("+tc.name+")" {
				t.Fatalf("the accepted save is not stored: %v", err)
			}
		})
	}
}

// The check runs in the transaction that applies the save: a change that
// commits after the request read the tag, and before its transaction starts, is
// not overwritten.
func TestUserUpdateChecksETagInsideTheTransaction(t *testing.T) {
	ctx := context.Background()
	f := newAPIFixtureOver(t, func(s storage.Storage) storage.Storage {
		return &commitBeforeTx{Storage: s, change: func(inner storage.Storage) {
			u, err := inner.GetUser(ctx, "target")
			if err == nil {
				u.SetName("Changed meanwhile")
				err = inner.UpsertUser(ctx, u)
			}
			if err != nil {
				t.Errorf("concurrent change: %v", err)
			}
		}}
	})
	admin := f.user(t, "admin", enum.RoleAdminID)
	f.user(t, "target", enum.RoleUserReaderID)
	tag := f.userETag(t, "target", admin)

	w := f.putUser("target", `{"role_ids":["`+enum.RoleUserEditorID+`"]}`, admin, tag)
	if w.Code != http.StatusPreconditionFailed || responseMessage(t, w) != staleUserMessage {
		t.Fatalf("save over a change that committed meanwhile: %d %s, want 412 %q", w.Code, w.Body.String(), staleUserMessage)
	}
	u, err := f.store.GetUser(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if u.Name() != "Changed meanwhile" || !slices.Equal(u.RoleIDsAll(), []string{enum.RoleUserReaderID}) {
		t.Fatalf("after the refused save: name %q roles %v, want the concurrent change only", u.Name(), u.RoleIDsAll())
	}
}

// Without If-Match, or with "*", an update takes the stored user as it is, as
// before: a role list from a stale view overwrites the stored one (the
// documented behaviour for API clients that never read first).
func TestUserUpdateWithoutIfMatchStillApplies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ifMatch []string
	}{{"no If-Match", nil}, {"If-Match *", []string{"*"}}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			ctx := context.Background()
			alice := f.user(t, "alice", enum.RoleAdminID)
			bob := f.user(t, "bob", enum.RoleAdminID)
			f.user(t, "target", enum.RoleUserReaderID)

			// Bob changes the roles after Alice loaded the user.
			if w := f.putUser("target", `{"role_ids":["`+enum.RoleUserEditorID+`"]}`, bob); w.Code != http.StatusNoContent {
				t.Fatalf("the first save: %d %s, want 204", w.Code, w.Body.String())
			}
			events := len(f.hub.RecentEvents(100))

			w := f.putUser("target", `{"name":"Alice","role_ids":["`+enum.RoleAgentReaderID+`"]}`, alice, tc.ifMatch...)
			if w.Code != http.StatusNoContent {
				t.Fatalf("save without a precondition: %d %s, want 204", w.Code, w.Body.String())
			}
			u, err := f.store.GetUser(ctx, "target")
			if err != nil {
				t.Fatal(err)
			}
			if u.Name() != "Alice" || !slices.Equal(u.RoleIDsAll(), []string{enum.RoleAgentReaderID}) {
				t.Fatalf("stored name %q roles %v, want the last save: Alice and [%s]", u.Name(), u.RoleIDsAll(), enum.RoleAgentReaderID)
			}
			if got := len(f.hub.RecentEvents(100)); got != events+1 {
				t.Fatalf("the save recorded %d events, want 1", got-events)
			}
		})
	}
}

// Every write of a user through the API moves the ETag; a read does not.
func TestUserETagFollowsEveryWrite(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user(t, "admin", enum.RoleAdminID)
	f.user(t, "target", enum.RoleUserReaderID)

	tag := f.userETag(t, "target", admin)
	if again := f.userETag(t, "target", admin); again != tag {
		t.Fatalf("a read moved the ETag from %s to %s", tag, again)
	}
	for _, tc := range []struct{ name, method, target, body string }{
		{"rename", http.MethodPut, "/api/v1/users/target", `{"name":"Renamed"}`},
		{"email", http.MethodPut, "/api/v1/users/target", `{"email":"target@example.com"}`},
		{"subject", http.MethodPut, "/api/v1/users/target", `{"subject":"renamed-subject"}`},
		{"roles", http.MethodPut, "/api/v1/users/target", `{"role_ids":["` + enum.RoleUserEditorID + `"]}`},
		{"permissions", http.MethodPut, "/api/v1/users/target", `{"permissions":["agents:get"]}`},
		{"disable", http.MethodPost, "/api/v1/users/target/disable", ""},
		{"enable", http.MethodPost, "/api/v1/users/target/enable", ""},
	} {
		if w := f.do(tc.method, tc.target, tc.body, admin); w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s, want 204", tc.name, w.Code, w.Body.String())
		}
		next := f.userETag(t, "target", admin)
		if next == tag {
			t.Fatalf("%s left the ETag as it was: %s", tc.name, tag)
		}
		tag = next
	}
}

// The edit dialog sends the ETag of the user it was rendered from as If-Match,
// whether or not it offers roles, and offers a reload when the answer is 412.
func TestUserEditFormSendsETag(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user(t, "admin", enum.RoleAdminID)
	f.user(t, "reader", enum.RoleUserReaderID)
	manager := f.user(t, "manager", enum.RoleUserAdminID)
	viewer := f.user(t, "viewer", enum.RoleUserReaderID)

	sends := func(page, tag string) int {
		return strings.Count(page, `'If-Match': `+jsexpr.String(tag))
	}
	rolesOffered := func(page string) bool { return strings.Contains(page, `fetch("/api/v1/roles")`) }

	// An admin edits another user: roles and permissions are in the form.
	tag := f.userETag(t, "reader", admin)
	page := html.UnescapeString(f.render(t, "/api/v1/users/reader", admin))
	if !rolesOffered(page) {
		t.Fatal("the form of another user does not offer roles")
	}
	if got := strings.Count(page, "If-Match"); got != 1 || sends(page, tag) != 1 {
		t.Fatalf("the edit form of another user sends If-Match %d times and the ETag %s %d times, want once each", got, tag, sends(page, tag))
	}

	// The form of the viewer's own account has no roles and sends it too.
	selfTag := f.userETag(t, "manager", manager)
	self := html.UnescapeString(f.render(t, "/api/v1/users/manager", manager))
	if rolesOffered(self) {
		t.Fatal("the form of the viewer's own account offers roles")
	}
	if got := strings.Count(self, "If-Match"); got != 1 || sends(self, selfTag) != 1 {
		t.Fatalf("the own-account form sends If-Match %d times and the ETag %s %d times, want once each", got, selfTag, sends(self, selfTag))
	}

	// A 412 shows the server's message and a link that reloads the user's page.
	if !strings.Contains(page, "stale = r.status === 412") {
		t.Fatal("the form does not note a 412 answer")
	}
	link := regexp.MustCompile(`<a [^>]*x-show="stale"[^>]*>[^<]*</a>`).FindString(page)
	if !strings.Contains(link, `href="/users/info/reader"`) || !strings.Contains(link, ">Reload</a>") {
		t.Fatalf("the form has no Reload link to the user's page: %q", link)
	}

	// A viewer who cannot edit has no form to send it.
	if got := html.UnescapeString(f.render(t, "/api/v1/users/reader", viewer)); strings.Contains(got, "If-Match") {
		t.Fatal("a viewer without users:edit is offered a form that sends If-Match")
	}

	// The dialog follows the stored user: after a save it carries the new ETag.
	if w := f.putUser("reader", `{"name":"Renamed"}`, admin, tag); w.Code != http.StatusNoContent {
		t.Fatalf("save: %d %s, want 204", w.Code, w.Body.String())
	}
	next := f.userETag(t, "reader", admin)
	after := html.UnescapeString(f.render(t, "/api/v1/users/reader", admin))
	if next == tag || sends(after, next) != 1 || sends(after, tag) != 0 {
		t.Fatalf("after a save the form sends the old ETag %d times and the new one %d times, want 0 and 1", sends(after, tag), sends(after, next))
	}
}
