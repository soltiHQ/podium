package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

func TestUserCreationSeparatelyAuthorizesAccessAssignments(t *testing.T) {
	st := inmemory.New()
	a := &API{userSVC: user.New(st, zerolog.Nop()), hub: event.NewHub(zerolog.Nop())}
	request := func(body string, permissions ...enum.Permission) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
		r = r.WithContext(transportctx.WithIdentity(r.Context(), &identity.Identity{UserID: "actor", Permissions: permissions}))
		w := httptest.NewRecorder()
		a.Users(w, r)
		return w.Code
	}
	for _, body := range []string{`{"subject":"forbidden","role_ids":["admin"]}`, `{"subject":"forbidden","permissions":["users:delete"]}`} {
		if got := request(body, enum.UsersAdd); got != 403 {
			t.Fatalf("forbidden assignment status %d", got)
		}
		if _, err := st.GetUserBySubject(context.Background(), "forbidden"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("forbidden request mutated users: %v", err)
		}
	}
	if got := request(`{"subject":"ordinary"}`, enum.UsersAdd); got != 204 {
		t.Fatalf("ordinary create status %d", got)
	}
	if got := request(`{"subject":"assigned","permissions":["agents:get"]}`, enum.UsersAdd, enum.UsersEdit, enum.AgentsGet); got != 204 {
		t.Fatalf("admin create status %d", got)
	}
	created, err := st.GetUserBySubject(context.Background(), "assigned")
	if err != nil {
		t.Fatal(err)
	}
	if got := created.PermissionsAll(); len(got) != 1 || got[0] != enum.AgentsGet {
		t.Fatalf("admin assignment lost: %+v", got)
	}
}

func TestUserEditorCannotGrantAdminRole(t *testing.T) {
	f := newAPIFixture(t)
	editor := f.user(t, "editor", enum.RoleUserEditorID)
	f.user(t, "reader", enum.RoleUserReaderID)
	ctx := context.Background()

	if w := f.do(http.MethodPost, "/api/v1/users", `{"subject":"mallory","role_ids":["001"]}`, editor); w.Code != http.StatusForbidden {
		t.Fatalf("create with the Admin role: %d %s, want 403", w.Code, w.Body.String())
	}
	if _, err := f.store.GetUserBySubject(ctx, "mallory"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("refused create stored the user: %v", err)
	}
	if w := f.do(http.MethodPost, "/api/v1/users", `{"subject":"eve","permissions":["users:delete"]}`, editor); w.Code != http.StatusForbidden {
		t.Fatalf("create with a permission the editor lacks: %d, want 403", w.Code)
	}
	if w := f.do(http.MethodPut, "/api/v1/users/reader", `{"role_ids":["001"]}`, editor); w.Code != http.StatusForbidden {
		t.Fatalf("promote to Admin: %d %s, want 403", w.Code, w.Body.String())
	}
	reader, err := f.store.GetUser(ctx, "reader")
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.RoleIDsAll(); !slices.Equal(got, []string{enum.RoleUserReaderID}) {
		t.Fatalf("refused promotion changed roles to %v", got)
	}
	if w := f.do(http.MethodPost, "/api/v1/users", `{"subject":"helper","role_ids":["013"]}`, editor); w.Code != http.StatusNoContent {
		t.Fatalf("create within the editor's permissions: %d %s, want 204", w.Code, w.Body.String())
	}
}

func TestUserEditorCannotResetAdminPassword(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	f.user(t, "user-admin", enum.RoleAdminID)
	f.user(t, "reader", enum.RoleUserReaderID)
	creds := credential.New(f.store, zerolog.Nop())
	if err := creds.SetPassword(ctx, credential.SetPasswordRequest{UserID: "user-admin", Password: "original-admin-password", Cost: 4}); err != nil {
		t.Fatal(err)
	}
	editor := f.user(t, "editor", enum.RoleUserEditorID)

	if w := f.do(http.MethodPost, "/api/v1/users/user-admin/password", `{"password":"attacker-chosen-password"}`, editor); w.Code != http.StatusForbidden {
		t.Fatalf("reset the Admin's password: %d %s, want 403", w.Code, w.Body.String())
	}
	c, err := f.store.GetCredentialByUserAndAuth(ctx, "user-admin", enum.Password)
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.store.GetVerifierByCredential(ctx, c.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.VerifyPassword(c, v, "original-admin-password"); err != nil {
		t.Fatalf("refused reset replaced the Admin's password: %v", err)
	}
	if w := f.do(http.MethodPost, "/api/v1/users/reader/password", `{"password":"reader-new-password"}`, editor); w.Code != http.StatusNoContent {
		t.Fatalf("reset a userReader's password: %d %s, want 204", w.Code, w.Body.String())
	}
}

// A userAdmin manages users but holds none of the agent or spec permissions,
// so every mutation of an Admin exceeds its ceiling.
func TestTargetCeilingCoversEveryUserMutation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPut, "/api/v1/users/admin", `{"name":"Renamed"}`},
		{http.MethodPost, "/api/v1/users/admin/disable", ""},
		{http.MethodPost, "/api/v1/users/admin/enable", ""},
		{http.MethodDelete, "/api/v1/users/admin", ""},
		{http.MethodPost, "/api/v1/users/admin/password", `{"password":"chosen-by-the-actor"}`},
		{http.MethodPost, "/api/v1/session/admin-session/revoke", ""},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			f := newAPIFixture(t)
			f.user(t, "admin", enum.RoleAdminID)
			f.session(t, "admin-session", "admin")
			actor := f.user(t, "user-manager", enum.RoleUserAdminID)
			if w := f.do(tc.method, tc.target, tc.body, actor); w.Code != http.StatusForbidden {
				t.Fatalf("%d %s, want 403", w.Code, w.Body.String())
			}
			admin, err := f.store.GetUser(ctx, "admin")
			if err != nil {
				t.Fatalf("refused request removed the Admin: %v", err)
			}
			if admin.Name() != "admin" || admin.Disabled() {
				t.Fatalf("refused request changed the Admin: name %q disabled %v", admin.Name(), admin.Disabled())
			}
			if _, err := f.store.GetCredentialByUserAndAuth(ctx, "admin", enum.Password); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("refused request set a password: %v", err)
			}
			if s, err := f.store.GetSession(ctx, "admin-session"); err != nil || s.Revoked() {
				t.Fatalf("refused request revoked or removed the Admin's session: %v", err)
			}
		})
	}
}

func TestActorMayManageSelfWithinExistingRules(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	me := f.user(t, "me", enum.RoleUserAdminID)

	if w := f.do(http.MethodPut, "/api/v1/users/me", `{"name":"New Name"}`, me); w.Code != http.StatusNoContent {
		t.Fatalf("own profile: %d %s, want 204", w.Code, w.Body.String())
	}
	if u, err := f.store.GetUser(ctx, "me"); err != nil || u.Name() != "New Name" {
		t.Fatalf("own profile not saved: %v", err)
	}
	if w := f.do(http.MethodPost, "/api/v1/users/me/password", `{"password":"my-new-long-password"}`, me); w.Code != http.StatusNoContent {
		t.Fatalf("own password: %d %s, want 204", w.Code, w.Body.String())
	}
	f.session(t, "my-session", "me") // set after the password change, which ends every session
	if w := f.do(http.MethodPost, "/api/v1/session/my-session/revoke", "", me); w.Code != http.StatusNoContent {
		t.Fatalf("own session: %d %s, want 204", w.Code, w.Body.String())
	}
	if s, err := f.store.GetSession(ctx, "my-session"); err != nil || !s.Revoked() {
		t.Fatalf("own session not revoked: %v", err)
	}
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPut, "/api/v1/users/me", `{"role_ids":["001"]}`},
		{http.MethodDelete, "/api/v1/users/me", ""},
		{http.MethodPost, "/api/v1/users/me/disable", ""},
	} {
		if w := f.do(tc.method, tc.target, tc.body, me); w.Code != http.StatusBadRequest {
			t.Fatalf("%s %s on yourself: %d, want 400", tc.method, tc.target, w.Code)
		}
	}
}

// Two Admins act on each other at the same time: the second request was
// authorized before its own account was deleted, so the Admin it targets is
// the last one left.
func TestCannotDeleteOrDisableLastAdmin(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, method, target, body string }{
		{"delete", http.MethodDelete, "/api/v1/users/first-admin", ""},
		{"disable", http.MethodPost, "/api/v1/users/first-admin/disable", ""},
		{"demote", http.MethodPut, "/api/v1/users/first-admin", `{"role_ids":["003"]}`},
		{"drop permissions", http.MethodPut, "/api/v1/users/first-admin", `{"role_ids":[],"permissions":["agents:get"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			first := f.user(t, "first-admin", enum.RoleAdminID)
			second := f.user(t, "second-admin", enum.RoleAdminID)
			if w := f.do(http.MethodDelete, "/api/v1/users/second-admin", "", first); w.Code != http.StatusNoContent {
				t.Fatalf("delete one of two Admins: %d %s, want 204", w.Code, w.Body.String())
			}
			if w := f.do(tc.method, tc.target, tc.body, second); w.Code != http.StatusConflict {
				t.Fatalf("%d %s, want 409", w.Code, w.Body.String())
			}
			u, err := f.store.GetUser(ctx, "first-admin")
			if err != nil {
				t.Fatalf("last Admin removed: %v", err)
			}
			if u.Disabled() || !slices.Equal(u.RoleIDsAll(), []string{enum.RoleAdminID}) || len(u.PermissionsAll()) != 0 {
				t.Fatalf("last Admin changed: disabled %v roles %v permissions %v", u.Disabled(), u.RoleIDsAll(), u.PermissionsAll())
			}
		})
	}
}

func TestUserDetailShowsOnlyActionsWithinViewerCeiling(t *testing.T) {
	f := newAPIFixture(t)
	f.user(t, "admin", enum.RoleAdminID)
	f.session(t, "admin-session", "admin")
	f.user(t, "reader", enum.RoleUserReaderID)
	f.session(t, "reader-session", "reader")
	manager := f.user(t, "manager", enum.RoleUserAdminID)

	admin := f.render(t, "/api/v1/users/admin", manager)
	for _, action := range []string{"/api/v1/users/admin/disable", "/api/v1/users/admin/password", "Edit user", "Delete user"} {
		if strings.Contains(admin, action) {
			t.Errorf("Admin detail offers %q to a userAdmin", action)
		}
	}
	if sessions := f.render(t, "/api/v1/users/admin/sessions", manager); strings.Contains(sessions, "/api/v1/session/admin-session/revoke") {
		t.Error("Admin sessions offer revoke to a userAdmin")
	}

	reader := f.render(t, "/api/v1/users/reader", manager)
	for _, action := range []string{"/api/v1/users/reader/disable", "/api/v1/users/reader/password", "Edit user", "Delete user"} {
		if !strings.Contains(reader, action) {
			t.Errorf("userReader detail hides %q from a userAdmin", action)
		}
	}
	if sessions := f.render(t, "/api/v1/users/reader/sessions", manager); !strings.Contains(sessions, "/api/v1/session/reader-session/revoke") {
		t.Error("userReader sessions hide revoke from a userAdmin")
	}
	viewer := f.user(t, "viewer", enum.RoleUserReaderID)
	if sessions := f.render(t, "/api/v1/users/reader/sessions", viewer); strings.Contains(sessions, "/revoke") {
		t.Error("sessions offer revoke to a viewer without users:edit")
	}
}
