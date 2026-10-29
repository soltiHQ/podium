package rbac

import (
	"context"
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type wrapStore struct {
	storage.Storage
	getRolesErr error
	getRolesRes []*model.Role
}

func (w wrapStore) GetRoles(ctx context.Context, ids []string) ([]*model.Role, error) {
	if w.getRolesErr != nil {
		return nil, w.getRolesErr
	}
	if w.getRolesRes != nil {
		return w.getRolesRes, nil
	}
	return w.Storage.GetRoles(ctx, ids)
}

func mustUser(t *testing.T, id, subject string) *model.User {
	t.Helper()
	u, err := model.NewUser(id, subject)
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	return u
}

func mustAddUserPerm(t *testing.T, u *model.User, p enum.Permission) {
	t.Helper()
	if err := u.PermissionAdd(p); err != nil {
		t.Fatalf("user.PermissionAdd(%q): %v", p, err)
	}
}

func mustAddUserRole(t *testing.T, u *model.User, roleID string) {
	t.Helper()
	if err := u.RoleAdd(roleID); err != nil {
		t.Fatalf("user.RoleAdd(%q): %v", roleID, err)
	}
}

func mustRole(t *testing.T, id, name string, perms ...enum.Permission) *model.Role {
	t.Helper()
	r, err := model.NewRole(id, name)
	if err != nil {
		t.Fatalf("NewRole: %v", err)
	}
	for _, p := range perms {
		if p == "" {
			continue
		}
		if err := r.PermissionAdd(p); err != nil {
			t.Fatalf("role.PermissionAdd(%q): %v", p, err)
		}
	}
	return r
}

func mustUpsertRole(t *testing.T, ctx context.Context, store storage.Storage, r *model.Role) {
	t.Helper()
	if err := store.UpsertRole(ctx, r); err != nil {
		t.Fatalf("UpsertRole: %v", err)
	}
}

func assertPerms(t *testing.T, got []enum.Permission, want ...enum.Permission) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected non-nil slice, got nil")
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d perms, got %d: %#v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d: expected %q, got %q; full=%#v", i, want[i], got[i], got)
		}
	}
}

func TestResolver_ResolveUserPermissions_InvalidArgs(t *testing.T) {
	ctx := context.Background()

	t.Run("nil resolver", func(t *testing.T) {
		var r *Resolver
		_, err := r.ResolveUserPermissions(ctx, mustUser(t, "u1", "s1"))
		if !errors.Is(err, auth.ErrInvalidArgument) {
			t.Fatalf("expected ErrInvalidArgument, got %v", err)
		}
	})

	t.Run("nil store", func(t *testing.T) {
		r := &Resolver{store: nil}
		_, err := r.ResolveUserPermissions(ctx, mustUser(t, "u1", "s1"))
		if !errors.Is(err, auth.ErrInvalidArgument) {
			t.Fatalf("expected ErrInvalidArgument, got %v", err)
		}
	})

	t.Run("nil user", func(t *testing.T) {
		r := NewResolver(inmemory.New())
		_, err := r.ResolveUserPermissions(ctx, nil)
		if !errors.Is(err, auth.ErrInvalidArgument) {
			t.Fatalf("expected ErrInvalidArgument, got %v", err)
		}
	})
}

func TestResolver_ResolveUserPermissions_Empty(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	r := NewResolver(store)

	u := mustUser(t, "u1", "s1")

	got, err := r.ResolveUserPermissions(ctx, u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("expected empty slice, got %#v", got)
	}
}

func TestResolver_ResolveUserPermissions_UserOnly_Dedup_Sorted(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	r := NewResolver(store)

	u := mustUser(t, "u1", "s1")

	mustAddUserPerm(t, u, "perm:b")
	mustAddUserPerm(t, u, "perm:a")

	_ = u.PermissionAdd("perm:a")

	got, err := r.ResolveUserPermissions(ctx, u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertPerms(t, got, "perm:a", "perm:b")
}

func TestResolver_ResolveUserPermissions_RolesOnly_Union_Sorted(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	r := NewResolver(store)

	u := mustUser(t, "u1", "s1")
	mustAddUserRole(t, u, "role-1")
	mustAddUserRole(t, u, "role-2")

	role1 := mustRole(t, "role-1", "r1", "perm:b", "perm:a")
	role2 := mustRole(t, "role-2", "r2", "perm:c")
	mustUpsertRole(t, ctx, store, role1)
	mustUpsertRole(t, ctx, store, role2)

	got, err := r.ResolveUserPermissions(ctx, u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertPerms(t, got, "perm:a", "perm:b", "perm:c")
}

func TestResolver_ResolveUserPermissions_UserAndRoles_Dedup_Sorted(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	r := NewResolver(store)

	u := mustUser(t, "u1", "s1")
	mustAddUserPerm(t, u, "perm:b")
	mustAddUserPerm(t, u, "perm:a")
	mustAddUserRole(t, u, "role-1")

	role1 := mustRole(t, "role-1", "r1", "perm:b", "perm:c")
	mustUpsertRole(t, ctx, store, role1)

	got, err := r.ResolveUserPermissions(ctx, u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertPerms(t, got, "perm:a", "perm:b", "perm:c")
}

func TestResolver_ResolveUserPermissions_IgnoresNilRoles(t *testing.T) {
	ctx := context.Background()

	base := inmemory.New()
	r := &Resolver{store: wrapStore{
		Storage:     base,
		getRolesRes: []*model.Role{nil},
	}}

	u := mustUser(t, "u1", "s1")
	mustAddUserRole(t, u, "role-1")

	got, err := r.ResolveUserPermissions(ctx, u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("expected empty slice, got %#v", got)
	}
}

func TestResolver_ResolveUserPermissions_PropagatesStorageErrors(t *testing.T) {
	ctx := context.Background()

	base := inmemory.New()
	r := &Resolver{store: wrapStore{
		Storage:     base,
		getRolesErr: storage.ErrUnavailable,
	}}

	u := mustUser(t, "u1", "s1")
	mustAddUserRole(t, u, "role-1")

	_, err := r.ResolveUserPermissions(ctx, u)
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}
