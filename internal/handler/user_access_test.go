package handler

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
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
		r.Header.Set("Content-Type", "application/json")
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

// Permission names were not validated before, and a name can leave enum.All
// later. Such a string grants nothing, so it must not shield its holder.
func TestUnknownPermissionsDoNotLockUsersOut(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	admin := f.user(t, "admin", enum.RoleAdminID)
	legacy, err := model.NewUser("legacy", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	legacy.SetPermissions([]string{string(enum.AgentsGet), "legacy:perm"})
	if err := f.store.UpsertUser(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	f.session(t, "legacy-session", "legacy")

	page := f.render(t, "/api/v1/users/legacy", admin)
	for _, action := range []string{"/api/v1/users/legacy/disable", "/api/v1/users/legacy/password", "Edit user", "Delete user"} {
		if !strings.Contains(page, action) {
			t.Errorf("legacy user detail hides %q from an Admin", action)
		}
	}
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPut, "/api/v1/users/legacy", `{"name":"Legacy"}`},
		{http.MethodPost, "/api/v1/users/legacy/disable", ""},
		{http.MethodPost, "/api/v1/users/legacy/enable", ""},
		{http.MethodPost, "/api/v1/session/legacy-session/revoke", ""},
		{http.MethodPut, "/api/v1/users/legacy", `{"permissions":["agents:get"]}`},
	} {
		if w := f.do(tc.method, tc.target, tc.body, admin); w.Code != http.StatusNoContent {
			t.Fatalf("%s %s: %d %s, want 204", tc.method, tc.target, w.Code, w.Body.String())
		}
	}
	u, err := f.store.GetUser(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.PermissionsAll(); !slices.Equal(got, []enum.Permission{enum.AgentsGet}) || u.Name() != "Legacy" || u.Disabled() {
		t.Fatalf("legacy user after cleanup: permissions %v name %q disabled %v", got, u.Name(), u.Disabled())
	}
	if w := f.do(http.MethodDelete, "/api/v1/users/legacy", "", admin); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s, want 204", w.Code, w.Body.String())
	}
}

func TestUnknownPermissionInputIsBadRequest(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	admin := f.user(t, "admin", enum.RoleAdminID)
	f.user(t, "reader", enum.RoleUserReaderID)
	if w := f.do(http.MethodPost, "/api/v1/users", `{"subject":"newcomer","permissions":["users:get","legacy:perm"]}`, admin); w.Code != http.StatusBadRequest {
		t.Fatalf("create with an unknown permission: %d %s, want 400", w.Code, w.Body.String())
	}
	if _, err := f.store.GetUserBySubject(ctx, "newcomer"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("refused create stored the user: %v", err)
	}
	if w := f.do(http.MethodPut, "/api/v1/users/reader", `{"permissions":["legacy:perm"]}`, admin); w.Code != http.StatusBadRequest {
		t.Fatalf("update with an unknown permission: %d %s, want 400", w.Code, w.Body.String())
	}
	if u, err := f.store.GetUser(ctx, "reader"); err != nil || len(u.PermissionsAll()) != 0 {
		t.Fatalf("refused update changed the user: %v", err)
	}
}

// The edit modal loads its role and permission options from the API; they
// must hold only what the viewer may grant.
func TestUserEditModalOffersOnlyGrantableAccess(t *testing.T) {
	f := newAPIFixture(t)
	f.user(t, "reader", enum.RoleUserReaderID)
	editor := f.user(t, "editor", enum.RoleUserEditorID)
	admin := f.user(t, "admin", enum.RoleAdminID)

	var endpoints []string
	page := html.UnescapeString(f.render(t, "/api/v1/users/reader", editor))
	for _, m := range regexp.MustCompile(`fetch\("([^"]+)"\)\.then`).FindAllStringSubmatch(page, -1) {
		endpoints = append(endpoints, m[1])
	}
	slices.Sort(endpoints)
	if want := []string{"/api/v1/permissions", "/api/v1/roles"}; !slices.Equal(endpoints, want) {
		t.Fatalf("edit modal loads options from %v, want %v", endpoints, want)
	}
	get := func(endpoint string, viewer *identity.Identity, body any) {
		t.Helper()
		w := f.do(http.MethodGet, endpoint, "", viewer)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", endpoint, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), body); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		viewer             *identity.Identity
		roles, permissions []string
	}{
		{editor, []string{"012", "013"}, []string{"users:add", "users:edit", "users:get"}},
		{admin, []string{"001", "002", "003", "011", "012", "013", "021", "022", "023", "031", "032"}, []string{
			"agents:edit", "agents:get", "taskspecs:add", "taskspecs:delete", "taskspecs:deploy", "taskspecs:edit",
			"taskspecs:get", "users:add", "users:delete", "users:edit", "users:get",
		}},
	} {
		var roles restv1.ListRolesResponse
		get("/api/v1/roles", tc.viewer, &roles)
		var roleIDs []string
		for _, r := range roles.Items {
			roleIDs = append(roleIDs, r.ID)
		}
		slices.Sort(roleIDs)
		if !slices.Equal(roleIDs, tc.roles) {
			t.Errorf("%s: role options %v, want %v", tc.viewer.UserID, roleIDs, tc.roles)
		}
		var perms restv1.ListPermissionsResponse
		get("/api/v1/permissions", tc.viewer, &perms)
		slices.Sort(perms.Items)
		if !slices.Equal(perms.Items, tc.permissions) {
			t.Errorf("%s: permission options %v, want %v", tc.viewer.UserID, perms.Items, tc.permissions)
		}
	}
}

// A new password needs 12 to 72 characters: 12 counted as characters, 72 as
// bytes (bcrypt's limit). Shorter or longer is a 400 that stores nothing.
func TestPasswordPolicy(t *testing.T) {
	ctx := context.Background()
	actor := &identity.Identity{UserID: "actor", Permissions: []enum.Permission{enum.UsersEdit}}
	for _, tc := range []struct {
		name     string
		password string
		want     int
	}{
		{"11 bytes", strings.Repeat("a", 11), http.StatusBadRequest},
		{"73 bytes", strings.Repeat("a", 73), http.StatusBadRequest},
		{"12 bytes", strings.Repeat("a", 12), http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			f.user(t, "target")
			body, err := json.Marshal(restv1.SetPasswordRequest{Password: tc.password})
			if err != nil {
				t.Fatal(err)
			}
			w := f.do(http.MethodPost, "/api/v1/users/target/password", string(body), actor)
			if w.Code != tc.want {
				t.Fatalf("HTTP %d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			_, stored := f.store.GetCredentialByUserAndAuth(ctx, "target", enum.Password)
			if tc.want != http.StatusBadRequest {
				if stored != nil {
					t.Fatalf("accepted password not stored: %v", stored)
				}
				return
			}
			var got struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Message != "Password must be 12-72 characters." {
				t.Fatalf("message %q (%v), want %q", got.Message, err, "Password must be 12-72 characters.")
			}
			if !errors.Is(stored, storage.ErrNotFound) {
				t.Fatalf("rejected password was stored: %v", stored)
			}
		})
	}
}
