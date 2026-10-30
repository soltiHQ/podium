package user

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain"
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

func ptr[T any](v T) *T { return &v }

// commitBeforeTx is a store whose first transaction is preceded by change,
// committed on its own: another request that lands after this request was
// authorized and before its transaction starts.
type commitBeforeTx struct {
	storage.Storage
	change func(storage.Storage)
	once   sync.Once
}

func (s *commitBeforeTx) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	s.once.Do(func() { s.change(s.Storage) })
	return s.Storage.WithTx(ctx, fn)
}

// racing returns a service over st whose first transaction is preceded by
// the committed edit of user id.
func racing(t *testing.T, st storage.Storage, id string, edit func(*model.User)) *Service {
	t.Helper()
	return New(&commitBeforeTx{Storage: st, change: func(inner storage.Storage) {
		u, err := inner.GetUser(context.Background(), id)
		if err == nil {
			edit(u)
			err = inner.UpsertUser(context.Background(), u)
		}
		if err != nil {
			t.Errorf("concurrent edit of %s: %v", id, err)
		}
	}}, zerolog.Nop())
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

	if _, err := svc.Update(ctx, editor, "user-admin", UpdatePatch{Name: ptr("Renamed")}); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("rename an Admin = %v, want ErrPermissionCeiling", err)
	}
	if got := stored(t, st, "user-admin").Name(); got != "" {
		t.Fatalf("refused update renamed the Admin to %q", got)
	}

	if _, err := svc.Update(ctx, editor, "reader", UpdatePatch{RoleIDs: ptr([]string{enum.RoleAdminID})}); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("promote to Admin = %v, want ErrPermissionCeiling", err)
	}
	if got := stored(t, st, "reader").RoleIDsAll(); !slices.Equal(got, []string{enum.RoleUserReaderID}) {
		t.Fatalf("refused promotion changed roles to %v", got)
	}

	within := UpdatePatch{Name: ptr("Reader"), RoleIDs: ptr([]string{enum.RoleUserEditorID})}
	if _, err := svc.Update(ctx, editor, "reader", within); err != nil {
		t.Fatalf("update within the ceiling = %v", err)
	}
	if got := stored(t, st, "reader"); got.Name() != "Reader" || !slices.Equal(got.RoleIDsAll(), []string{enum.RoleUserEditorID}) {
		t.Fatalf("update within the ceiling not saved: %q %v", got.Name(), got.RoleIDsAll())
	}

	if _, err := svc.Update(ctx, nil, "reader", UpdatePatch{}); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("Update without an actor = %v, want ErrPermissionCeiling", err)
	}
	if _, err := svc.Update(ctx, editor, "missing", UpdatePatch{Name: ptr("Missing")}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Update of an unknown user = %v, want ErrNotFound", err)
	}
	if _, err := st.GetUser(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Update created a user: %v", err)
	}
	if _, err := svc.Update(ctx, editor, "", UpdatePatch{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("Update without an ID = %v, want ErrInvalidArgument", err)
	}
}

// The target ceiling looks at the user as stored, not at the result of the
// patch: an actor may not lower a user who holds more than the actor does.
func TestUpdateCannotDemoteAboveTheTargetCeiling(t *testing.T) {
	svc, st := fixture(t, map[string][]string{
		"first-admin":  {enum.RoleAdminID},
		"second-admin": {enum.RoleAdminID},
	})
	patch := UpdatePatch{RoleIDs: ptr([]string{enum.RoleUserReaderID})}
	if _, err := svc.Update(context.Background(), actor("editor", userEditorPerms...), "first-admin", patch); !errors.Is(err, ErrPermissionCeiling) {
		t.Fatalf("demote an Admin = %v, want ErrPermissionCeiling", err)
	}
	if got := stored(t, st, "first-admin").RoleIDsAll(); !slices.Equal(got, []string{enum.RoleAdminID}) {
		t.Fatalf("refused demotion changed the roles to %v", got)
	}
}

// profile is what an update may change, as stored.
type profile struct {
	subject, name, email string
	roles                []string
	perms                []enum.Permission
	disabled             bool
}

func profileOf(u *model.User) profile {
	return profile{u.Subject(), u.Name(), u.Email(), u.RoleIDsAll(), u.PermissionsAll(), u.Disabled()}
}

func (p profile) same(q profile) bool {
	return p.subject == q.subject && p.name == q.name && p.email == q.email && p.disabled == q.disabled &&
		slices.Equal(p.roles, q.roles) && slices.Equal(p.perms, q.perms)
}

// Update applies its patch to the user its transaction reads, so a change that
// commits after the request was authorized and before the transaction starts
// survives an update that does not set it.
func TestConcurrentDisableSurvivesUpdate(t *testing.T) {
	_, st := fixture(t, map[string][]string{"reader": {enum.RoleUserReaderID}})
	svc := racing(t, st, "reader", (*model.User).Disable)

	got, err := svc.Update(context.Background(), actor("editor", userEditorPerms...), "reader", UpdatePatch{Name: ptr("New Name")})
	if err != nil {
		t.Fatalf("name-only update = %v", err)
	}
	for _, u := range []*model.User{got, stored(t, st, "reader")} {
		if !u.Disabled() || u.Name() != "New Name" {
			t.Fatalf("update after a concurrent disable: disabled %v name %q, want true %q", u.Disabled(), u.Name(), "New Name")
		}
	}
}

// The member saves their own profile while an admin takes away a role or a
// direct permission: the profile save must not give it back.
func TestConcurrentDemotionSurvivesProfileSave(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*model.User)
		roles []string
		perms []enum.Permission
	}{
		{
			name:  "role removed",
			edit:  func(u *model.User) { u.RoleDelete(enum.RoleUserEditorID) },
			roles: []string{enum.RoleUserReaderID},
			perms: []enum.Permission{enum.UsersEdit},
		},
		{
			name:  "direct permission removed",
			edit:  func(u *model.User) { u.PermissionDelete(enum.UsersEdit) },
			roles: []string{enum.RoleUserReaderID, enum.RoleUserEditorID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st := fixture(t, map[string][]string{"member": {enum.RoleUserReaderID, enum.RoleUserEditorID}})
			member := stored(t, st, "member")
			member.SetPermissions([]string{string(enum.UsersEdit)})
			if err := st.UpsertUser(context.Background(), member); err != nil {
				t.Fatal(err)
			}
			svc := racing(t, st, "member", tc.edit)

			if _, err := svc.Update(context.Background(), actor("member", userEditorPerms...), "member", UpdatePatch{Name: ptr("Me")}); err != nil {
				t.Fatalf("profile save = %v", err)
			}
			got := stored(t, st, "member")
			if got.Name() != "Me" || !slices.Equal(got.RoleIDsAll(), tc.roles) || !slices.Equal(got.PermissionsAll(), tc.perms) {
				t.Fatalf("after the profile save: name %q roles %v permissions %v, want Me %v %v",
					got.Name(), got.RoleIDsAll(), got.PermissionsAll(), tc.roles, tc.perms)
			}
		})
	}
}

func TestUpdatePatchKeepsUnsetFields(t *testing.T) {
	editor := actor("editor", userEditorPerms...)
	base := profile{
		subject:  "target",
		name:     "Target",
		email:    "target@example.com",
		roles:    []string{enum.RoleUserReaderID},
		perms:    []enum.Permission{enum.UsersEdit},
		disabled: true,
	}
	for _, tc := range []struct {
		name  string
		patch UpdatePatch
		want  func(*profile)
	}{
		{"nothing", UpdatePatch{}, func(*profile) {}},
		{"name", UpdatePatch{Name: ptr("Renamed")}, func(p *profile) { p.name = "Renamed" }},
		{"empty name", UpdatePatch{Name: ptr("")}, func(p *profile) { p.name = "" }},
		{"email", UpdatePatch{Email: ptr("other@example.com")}, func(p *profile) { p.email = "other@example.com" }},
		{"empty email", UpdatePatch{Email: ptr("")}, func(p *profile) { p.email = "" }},
		{"subject", UpdatePatch{Subject: ptr("renamed")}, func(p *profile) { p.subject = "renamed" }},
		{"roles", UpdatePatch{RoleIDs: ptr([]string{enum.RoleUserEditorID})}, func(p *profile) { p.roles = []string{enum.RoleUserEditorID} }},
		{"no roles", UpdatePatch{RoleIDs: ptr([]string(nil))}, func(p *profile) { p.roles = nil }},
		{"permissions", UpdatePatch{Permissions: ptr([]string{string(enum.UsersGet)})}, func(p *profile) { p.perms = []enum.Permission{enum.UsersGet} }},
		{"no permissions", UpdatePatch{Permissions: ptr([]string{})}, func(p *profile) { p.perms = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st := fixture(t, map[string][]string{"target": base.roles})
			u := stored(t, st, "target")
			u.SetName(base.name)
			u.SetEmail(base.email)
			u.SetPermissions([]string{string(enum.UsersEdit)})
			u.Disable()
			if err := st.UpsertUser(context.Background(), u); err != nil {
				t.Fatal(err)
			}

			want := base
			tc.want(&want)
			got, err := svc.Update(context.Background(), editor, "target", tc.patch)
			if err != nil {
				t.Fatalf("Update = %v", err)
			}
			if p := profileOf(got); !p.same(want) {
				t.Fatalf("returned %+v, want %+v", p, want)
			}
			if p := profileOf(stored(t, st, "target")); !p.same(want) {
				t.Fatalf("stored %+v, want %+v", p, want)
			}
		})
	}
}

// Update runs the checks and the normalization Create and Upsert run, on the
// user the patch makes.
func TestUpdateNormalizesAndChecksTheMergedUser(t *testing.T) {
	ctx := context.Background()
	editor := actor("editor", userEditorPerms...)
	for _, tc := range []struct {
		name  string
		patch UpdatePatch
		want  error
	}{
		{"blank subject", UpdatePatch{Subject: ptr("  ")}, domain.ErrInvalidSubject},
		{"invalid email", UpdatePatch{Email: ptr("not-an-email")}, domain.ErrInvalidEmail},
		{"subject of another user", UpdatePatch{Subject: ptr("other")}, storage.ErrAlreadyExists},
		{"subject of another user in another case", UpdatePatch{Subject: ptr(" OTHER ")}, storage.ErrAlreadyExists},
		{"unknown role", UpdatePatch{RoleIDs: ptr([]string{"no-such-role"})}, storage.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st := fixture(t, map[string][]string{"target": {enum.RoleUserReaderID}, "other": nil})
			before := profileOf(stored(t, st, "target"))
			if _, err := svc.Update(ctx, editor, "target", tc.patch); !errors.Is(err, tc.want) {
				t.Fatalf("Update = %v, want %v", err, tc.want)
			}
			if after := profileOf(stored(t, st, "target")); !after.same(before) {
				t.Fatalf("refused update changed the user to %+v, was %+v", after, before)
			}
		})
	}

	svc, st := fixture(t, map[string][]string{"target": {enum.RoleUserReaderID}})
	patch := UpdatePatch{Subject: ptr("  Mixed.CASE "), Name: ptr("  Padded  "), Email: ptr(" Mixed@Example.COM ")}
	if _, err := svc.Update(ctx, editor, "target", patch); err != nil {
		t.Fatalf("Update = %v", err)
	}
	want := profile{subject: "mixed.case", name: "Padded", email: "mixed@example.com", roles: []string{enum.RoleUserReaderID}}
	if p := profileOf(stored(t, st, "target")); !p.same(want) {
		t.Fatalf("stored %+v, want %+v", p, want)
	}
	if _, err := svc.Update(ctx, editor, "target", UpdatePatch{Subject: ptr("MIXED.case")}); err != nil {
		t.Fatalf("keeping the own subject in another case = %v, want nil", err)
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
		run  func(*Service) error
	}{
		{"disable", func(svc *Service) error {
			_, err := svc.SetDisabled(ctx, admin, "admin", true)
			return err
		}},
		{"delete", func(svc *Service) error { return svc.Delete(ctx, admin, "admin") }},
		{"demote", func(svc *Service) error {
			_, err := svc.Update(ctx, admin, "admin", UpdatePatch{RoleIDs: ptr([]string{enum.RoleReaderID})})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st := fixture(t, map[string][]string{"admin": {enum.RoleAdminID}, "reader": {enum.RoleReaderID}})
			if err := tc.run(svc); !errors.Is(err, ErrLastAdmin) {
				t.Fatalf("last Admin: %v, want ErrLastAdmin", err)
			}
			if u := stored(t, st, "admin"); u.Disabled() || !slices.Equal(u.RoleIDsAll(), []string{enum.RoleAdminID}) {
				t.Fatalf("last Admin changed: disabled %v roles %v", u.Disabled(), u.RoleIDsAll())
			}
			if _, err := svc.Update(ctx, admin, "admin", UpdatePatch{Name: ptr("Still Admin")}); err != nil {
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
			if err := tc.run(svc); !errors.Is(err, ErrLastAdmin) {
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
			if err := tc.run(svc); err != nil {
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
	cleaned := UpdatePatch{Permissions: ptr([]string{string(enum.UsersGet)})}
	if _, err := svc.Update(ctx, editor, "legacy", cleaned); err != nil {
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
