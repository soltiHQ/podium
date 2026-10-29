package user

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

var (
	userAdminPerms  = []enum.Permission{enum.UsersGet, enum.UsersAdd, enum.UsersEdit, enum.UsersDelete}
	userEditorPerms = []enum.Permission{enum.UsersGet, enum.UsersAdd, enum.UsersEdit}
)

// fixture stores the built-in roles and one enabled user per entry (ID → role IDs).
func fixture(t *testing.T, users map[string][]string) (*Service, *inmemory.Store) {
	t.Helper()
	ctx := context.Background()
	st := inmemory.New()
	for _, br := range enum.BuiltinRoles {
		r, err := model.NewRole(br.ID, br.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range br.Permissions {
			if err := r.PermissionAdd(p); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.UpsertRole(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for id, roles := range users {
		u, err := model.NewUser(id, id)
		if err != nil {
			t.Fatal(err)
		}
		u.SetRoleIDs(roles)
		if err := st.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	return New(st, zerolog.Nop()), st
}

func actor(userID string, perms ...enum.Permission) *identity.Identity {
	return &identity.Identity{UserID: userID, Permissions: perms}
}

func stored(t *testing.T, st storage.Storage, id string) *model.User {
	t.Helper()
	u, err := st.GetUser(context.Background(), id)
	if err != nil {
		t.Fatalf("user %s: %v", id, err)
	}
	return u
}

func TestEffectivePermissionsUnionsRolesAndDirectGrants(t *testing.T) {
	_, st := fixture(t, nil)
	u, err := model.NewUser("mixed", "mixed")
	if err != nil {
		t.Fatal(err)
	}
	u.SetRoleIDs([]string{enum.RoleUserReaderID, enum.RoleAgentReaderID})
	u.SetPermissions([]string{string(enum.SpecsDeploy), string(enum.UsersGet)})
	got, err := EffectivePermissions(context.Background(), st, u)
	if err != nil {
		t.Fatal(err)
	}
	want := map[enum.Permission]struct{}{enum.UsersGet: {}, enum.AgentsGet: {}, enum.SpecsDeploy: {}}
	if !maps.Equal(got, want) {
		t.Fatalf("EffectivePermissions = %v, want %v", got, want)
	}
	bare, err := model.NewUser("bare", "bare")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := EffectivePermissions(context.Background(), st, bare); err != nil || len(got) != 0 {
		t.Fatalf("user without grants: %v %v, want none", got, err)
	}
}

// A request resolves the actor's permissions once; the user may gain more
// before the transaction runs. Acting on yourself stays allowed.
func TestCheckTargetExemptsYourself(t *testing.T) {
	_, st := fixture(t, map[string][]string{"me": {enum.RoleAdminID}})
	ctx := context.Background()
	me := stored(t, st, "me")
	if err := CheckTarget(ctx, st, actor("me", enum.UsersEdit), me); err != nil {
		t.Fatalf("acting on yourself = %v, want nil", err)
	}
	if err := CheckTarget(ctx, st, actor("other", enum.UsersEdit), me); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("acting on a stronger user = %v, want ErrPermissionCeiling", err)
	}
	if err := CheckTarget(ctx, st, nil, me); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("no actor = %v, want ErrPermissionCeiling", err)
	}
	if err := CheckTarget(ctx, st, actor("other"), nil); err != nil {
		t.Fatalf("no target = %v, want nil", err)
	}
}

func TestCreateKeepsGrantsWithinActorCeiling(t *testing.T) {
	svc, st := fixture(t, map[string][]string{"user-admin": {enum.RoleAdminID}})
	ctx := context.Background()
	editor := actor("editor", userEditorPerms...)
	for _, tc := range []struct {
		id           string
		roles, perms []string
		want         error
	}{
		{"admin-role", []string{enum.RoleAdminID}, nil, ErrPermissionCeiling},
		{"user-admin-role", []string{enum.RoleUserAdminID}, nil, ErrPermissionCeiling},
		{"direct-delete", nil, []string{string(enum.UsersDelete)}, ErrPermissionCeiling},
		{"reader-role", []string{enum.RoleUserReaderID}, nil, nil},
		{"direct-add", nil, []string{string(enum.UsersAdd)}, nil},
		{"nothing", nil, nil, nil},
	} {
		u, err := model.NewUser(tc.id, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		u.SetRoleIDs(tc.roles)
		u.SetPermissions(tc.perms)
		if err := svc.Create(ctx, editor, u); !errors.Is(err, tc.want) {
			t.Errorf("%s: Create = %v, want %v", tc.id, err, tc.want)
		}
		_, err = st.GetUser(ctx, tc.id)
		if created := err == nil; created != (tc.want == nil) {
			t.Errorf("%s: stored = %v, want %v", tc.id, created, tc.want == nil)
		}
	}

	u, err := model.NewUser("nobody", "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, nil, u); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("Create without an actor = %v, want ErrPermissionCeiling", err)
	}

	// Create must not replace an existing user: that would skip the target ceiling.
	clobber, err := model.NewUser("user-admin", "user-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, editor, clobber); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("Create over an existing user = %v, want ErrAlreadyExists", err)
	}
	if got := stored(t, st, "user-admin").RoleIDsAll(); !slices.Equal(got, []string{enum.RoleAdminID}) {
		t.Fatalf("Create replaced the Admin's roles with %v", got)
	}
}

func TestUpdateEnforcesTargetAndGrantCeilings(t *testing.T) {
	svc, st := fixture(t, map[string][]string{
		"user-admin": {enum.RoleAdminID},
		"reader":     {enum.RoleUserReaderID},
	})
	ctx := context.Background()
	editor := actor("editor", userEditorPerms...)

	admin := stored(t, st, "user-admin")
	admin.SetName("Renamed")
	if err := svc.Update(ctx, editor, admin); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("rename an Admin = %v, want ErrPermissionCeiling", err)
	}
	if got := stored(t, st, "user-admin").Name(); got != "" {
		t.Fatalf("refused update renamed the Admin to %q", got)
	}

	promoted := stored(t, st, "reader")
	promoted.SetRoleIDs([]string{enum.RoleAdminID})
	if err := svc.Update(ctx, editor, promoted); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("promote to Admin = %v, want ErrPermissionCeiling", err)
	}
	if got := stored(t, st, "reader").RoleIDsAll(); !slices.Equal(got, []string{enum.RoleUserReaderID}) {
		t.Fatalf("refused promotion changed roles to %v", got)
	}

	renamed := stored(t, st, "reader")
	renamed.SetName("Reader")
	renamed.SetRoleIDs([]string{enum.RoleUserEditorID})
	if err := svc.Update(ctx, editor, renamed); err != nil {
		t.Fatalf("update within the ceiling = %v", err)
	}
	if got := stored(t, st, "reader"); got.Name() != "Reader" || !slices.Equal(got.RoleIDsAll(), []string{enum.RoleUserEditorID}) {
		t.Fatalf("update within the ceiling not saved: %q %v", got.Name(), got.RoleIDsAll())
	}

	missing, err := model.NewUser("missing", "missing")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Update(ctx, editor, missing); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Update of an unknown user = %v, want ErrNotFound", err)
	}
	if _, err := st.GetUser(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Update created a user: %v", err)
	}
}

func TestStatusAndDeleteEnforceTargetCeiling(t *testing.T) {
	svc, st := fixture(t, map[string][]string{
		"user-admin": {enum.RoleAdminID},
		"reader":     {enum.RoleUserReaderID},
	})
	ctx := context.Background()
	manager := actor("manager", userAdminPerms...)

	for _, disabled := range []bool{true, false} {
		if _, err := svc.SetDisabled(ctx, manager, "user-admin", disabled); !errors.Is(err, ErrPermissionCeiling) {
			t.Fatalf("SetDisabled(Admin, %v) = %v, want ErrPermissionCeiling", disabled, err)
		}
	}
	if err := svc.Delete(ctx, manager, "user-admin"); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("Delete(Admin) = %v, want ErrPermissionCeiling", err)
	}
	if stored(t, st, "user-admin").Disabled() {
		t.Fatal("refused SetDisabled disabled the Admin")
	}

	u, err := svc.SetDisabled(ctx, manager, "reader", true)
	if err != nil || !u.Disabled() || !stored(t, st, "reader").Disabled() {
		t.Fatalf("disable within the ceiling: %v", err)
	}
	if u, err := svc.SetDisabled(ctx, manager, "reader", false); err != nil || u.Disabled() || stored(t, st, "reader").Disabled() {
		t.Fatalf("enable within the ceiling: %v", err)
	}
	if err := svc.Delete(ctx, manager, "reader"); err != nil {
		t.Fatalf("delete within the ceiling: %v", err)
	}
	if _, err := st.GetUser(ctx, "reader"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted user still stored: %v", err)
	}
	if _, err := svc.SetDisabled(ctx, manager, "reader", true); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("SetDisabled of a deleted user = %v, want ErrNotFound", err)
	}
}

// The handlers refuse these actions on yourself, so the last Admin acts on
// itself here to reach the invariant directly.
func TestLastAdminKeepsEveryPermission(t *testing.T) {
	ctx := context.Background()
	admin := actor("admin", enum.All...)
	for _, tc := range []struct {
		name string
		run  func(*Service, storage.Storage) error
	}{
		{"disable", func(svc *Service, _ storage.Storage) error {
			_, err := svc.SetDisabled(ctx, admin, "admin", true)
			return err
		}},
		{"delete", func(svc *Service, _ storage.Storage) error { return svc.Delete(ctx, admin, "admin") }},
		{"demote", func(svc *Service, st storage.Storage) error {
			u := stored(t, st, "admin")
			u.SetRoleIDs([]string{enum.RoleReaderID})
			return svc.Update(ctx, admin, u)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st := fixture(t, map[string][]string{"admin": {enum.RoleAdminID}, "reader": {enum.RoleReaderID}})
			if err := tc.run(svc, st); !errors.Is(err, ErrLastAdmin) {
				t.Fatalf("last Admin: %v, want ErrLastAdmin", err)
			}
			if u := stored(t, st, "admin"); u.Disabled() || !slices.Equal(u.RoleIDsAll(), []string{enum.RoleAdminID}) {
				t.Fatalf("last Admin changed: disabled %v roles %v", u.Disabled(), u.RoleIDsAll())
			}
			renamed := stored(t, st, "admin")
			renamed.SetName("Still Admin")
			if err := svc.Update(ctx, admin, renamed); err != nil {
				t.Fatalf("renaming the last Admin = %v, want nil", err)
			}

			// A disabled user with every permission does not count.
			off, err := model.NewUser("disabled-admin", "disabled-admin")
			if err != nil {
				t.Fatal(err)
			}
			off.SetRoleIDs([]string{enum.RoleAdminID})
			off.Disable()
			if err := st.UpsertUser(ctx, off); err != nil {
				t.Fatal(err)
			}
			if err := tc.run(svc, st); !errors.Is(err, ErrLastAdmin) {
				t.Fatalf("with only a disabled Admin left: %v, want ErrLastAdmin", err)
			}

			// Every permission granted directly makes another Admin.
			direct, err := model.NewUser("direct-admin", "direct-admin")
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range enum.All {
				if err := direct.PermissionAdd(p); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.UpsertUser(ctx, direct); err != nil {
				t.Fatal(err)
			}
			if err := tc.run(svc, st); err != nil {
				t.Fatalf("with another enabled Admin: %v", err)
			}
		})
	}
}

// A string outside enum.All grants nothing, so it neither shields the user
// who holds it nor stops an actor from granting a role that carries it.
func TestUnknownPermissionsNeitherBlockNorGrant(t *testing.T) {
	svc, st := fixture(t, nil)
	ctx := context.Background()
	editor := actor("editor", userEditorPerms...)

	legacy, err := model.NewUser("legacy", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	legacy.SetPermissions([]string{string(enum.UsersGet), "legacy:perm"})
	if err := st.UpsertUser(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := CheckTarget(ctx, st, editor, stored(t, st, "legacy")); err != nil {
		t.Fatalf("manage a user with an unknown permission = %v, want nil", err)
	}
	cleaned := stored(t, st, "legacy")
	cleaned.SetPermissions([]string{string(enum.UsersGet)})
	if err := svc.Update(ctx, editor, cleaned); err != nil {
		t.Fatalf("remove the unknown permission = %v, want nil", err)
	}
	if got := stored(t, st, "legacy").PermissionsAll(); !slices.Equal(got, []enum.Permission{enum.UsersGet}) {
		t.Fatalf("permissions after cleanup %v, want [users:get]", got)
	}
	if _, err := svc.SetDisabled(ctx, editor, "legacy", true); err != nil {
		t.Fatalf("disable = %v, want nil", err)
	}

	role, err := model.NewRole("legacy-role", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []enum.Permission{enum.UsersGet, "legacy:perm"} {
		if err := role.PermissionAdd(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	u, err := model.NewUser("assigned", "assigned")
	if err != nil {
		t.Fatal(err)
	}
	u.SetRoleIDs([]string{"legacy-role"})
	if err := svc.Create(ctx, editor, u); err != nil {
		t.Fatalf("grant a role carrying an unknown permission = %v, want nil", err)
	}
}

func TestCanGrantSkipsOnlyUnknownPermissions(t *testing.T) {
	editor := actor("editor", userEditorPerms...)
	for _, tc := range []struct {
		actor *identity.Identity
		perms []enum.Permission
		want  bool
	}{
		{editor, nil, true},
		{editor, []enum.Permission{enum.UsersGet, enum.UsersEdit}, true},
		{editor, []enum.Permission{"legacy:perm", enum.UsersGet}, true},
		{editor, []enum.Permission{"legacy:perm", enum.AgentsGet}, false},
		{editor, []enum.Permission{enum.UsersGet, enum.UsersDelete}, false},
		{actor("odd", "legacy:perm"), []enum.Permission{enum.AgentsGet}, false},
		{nil, nil, false},
	} {
		if got := CanGrant(tc.actor, tc.perms...); got != tc.want {
			t.Errorf("CanGrant(%v, %v) = %v, want %v", tc.actor, tc.perms, got, tc.want)
		}
	}
}
