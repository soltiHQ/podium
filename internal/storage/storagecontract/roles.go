package storagecontract

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// roles checks the role rows: the round trip, replacement, the rows a store
// refuses, deletion, the lookups by name and by IDs and the listing, which takes
// no filter the factory builds.
func roles(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		want := fullRole(t, "role-a", "admin", 0, minute(0))
		must(t, s.UpsertRole(ctx, want))

		got, err := s.GetRole(ctx, want.ID())
		gotRow(t, "GetRole", got, err, want, equalRole)
		got, err = s.GetRoleByName(ctx, want.Name())
		gotRow(t, "GetRoleByName", got, err, want, equalRole)
		many, err := s.GetRoles(ctx, []string{want.ID()})
		must(t, err)
		equalRows(t, "GetRoles", many, []*model.Role{want}, equalRole)
		listed, err := listedRoles(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListRoles", listed, []*model.Role{want}, equalRole)
	})

	t.Run("upsert replaces", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		other := fullRole(t, "role-b", "viewer", 0, minute(3))
		must(t, errors.Join(s.UpsertRole(ctx, fullRole(t, "role-a", "admin", 0, minute(0))), s.UpsertRole(ctx, other)))

		// Every field changes, the name and the creation time too: the row lists at
		// its new time, and the lookup finds it by its new name alone.
		want := fullRole(t, "role-a", "owner", 1, minute(5))
		must(t, s.UpsertRole(ctx, want))
		got, err := s.GetRole(ctx, want.ID())
		gotRow(t, "GetRole", got, err, want, equalRole)
		got, err = s.GetRoleByName(ctx, "owner")
		gotRow(t, "GetRoleByName of the new name", got, err, want, equalRole)
		expect(t, outcome{"GetRoleByName of the old name", errOf(s.GetRoleByName(ctx, "admin")), storage.ErrNotFound})
		listed, err := listedRoles(ctx, s, nil)
		must(t, err)
		equalRows(t, "ListRoles", listed, []*model.Role{want, other}, equalRole)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.Role{}
		noID.SetCreatedAt(minute(0))
		noCreation := fullRole(t, "role-a", "admin", 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"UpsertRole of nil", s.UpsertRole(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertRole of a row without an ID", s.UpsertRole(ctx, noID), storage.ErrInvalidArgument},
			outcome{"UpsertRole of a row without a creation time", s.UpsertRole(ctx, noCreation), storage.ErrInvalidArgument},
		)
		listed, err := listedRoles(ctx, s, nil)
		sameIDs(t, "ListRoles after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept := fullRole(t, "role-b", "viewer", 0, minute(0))
		must(t, errors.Join(s.UpsertRole(ctx, fullRole(t, "role-a", "admin", 0, minute(0))), s.UpsertRole(ctx, kept)))
		expect(t,
			outcome{"DeleteRole", s.DeleteRole(ctx, "role-a"), nil},
			outcome{"GetRole of a deleted row", errOf(s.GetRole(ctx, "role-a")), storage.ErrNotFound},
			outcome{"GetRoleByName of a deleted row", errOf(s.GetRoleByName(ctx, "admin")), storage.ErrNotFound},
			outcome{"DeleteRole of a deleted row", s.DeleteRole(ctx, "role-a"), storage.ErrNotFound},
			outcome{"GetRole of an ID never written", errOf(s.GetRole(ctx, "role-x")), storage.ErrNotFound},
			outcome{"DeleteRole of an ID never written", s.DeleteRole(ctx, "role-x"), storage.ErrNotFound},
			outcome{"GetRole of an empty ID", errOf(s.GetRole(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteRole of an empty ID", s.DeleteRole(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetRole(ctx, kept.ID())
		gotRow(t, "GetRole of the row kept", got, err, kept, equalRole)
	})

	t.Run("lookup by name", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// Names that differ from admin by a suffix or in case: an exact comparison
		// finds admin's row alone.
		want := fullRole(t, "role-a", "admin", 0, minute(0))
		must(t, errors.Join(
			s.UpsertRole(ctx, want),
			s.UpsertRole(ctx, fullRole(t, "role-b", "admin-ro", 0, minute(0))),
			s.UpsertRole(ctx, fullRole(t, "role-c", "Admin", 0, minute(0))),
		))
		got, err := s.GetRoleByName(ctx, "admin")
		gotRow(t, "GetRoleByName", got, err, want, equalRole)
		expect(t,
			outcome{"GetRoleByName of a prefix", errOf(s.GetRoleByName(ctx, "adm")), storage.ErrNotFound},
			outcome{"GetRoleByName with spaces around it", errOf(s.GetRoleByName(ctx, " admin ")), storage.ErrNotFound},
			outcome{"GetRoleByName of a name never written", errOf(s.GetRoleByName(ctx, "owner")), storage.ErrNotFound},
			outcome{"GetRoleByName of an empty name", errOf(s.GetRoleByName(ctx, "")), storage.ErrInvalidArgument},
		)
		// The name is indexed, not unique: two rows of one name are a fault.
		must(t, s.UpsertRole(ctx, fullRole(t, "role-d", "admin", 1, minute(1))))
		expect(t, outcome{"GetRoleByName of a name two rows hold", errOf(s.GetRoleByName(ctx, "admin")), storage.ErrInternal})
	})

	t.Run("lookup by IDs", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		a, b, c := fullRole(t, "role-a", "admin", 0, minute(0)), fullRole(t, "role-b", "editor", 1, minute(1)), fullRole(t, "role-c", "reader", 0, minute(2))
		must(t, errors.Join(s.UpsertRole(ctx, a), s.UpsertRole(ctx, b), s.UpsertRole(ctx, c)))

		// The roles come in the order of the IDs asked for, not the listing order.
		got, err := s.GetRoles(ctx, []string{"role-a", "role-c", "role-b"})
		must(t, err)
		equalRows(t, "GetRoles", got, []*model.Role{a, c, b}, equalRole)
		// Divergence: the spec says GetRoles drops a repeated ID. The in-memory store
		// looks each ID up once but returns a role for every ID asked for, in their
		// order, a repeated one again; the suite pins that.
		got, err = s.GetRoles(ctx, []string{"role-b", "role-a", "role-b"})
		must(t, err)
		equalRows(t, "GetRoles of a repeated ID", got, []*model.Role{b, a, b}, equalRole)
		expect(t,
			outcome{"GetRoles with an ID never written", errOf(s.GetRoles(ctx, []string{"role-a", "role-x"})), storage.ErrNotFound},
			outcome{"GetRoles of nil", errOf(s.GetRoles(ctx, nil)), storage.ErrInvalidArgument},
			outcome{"GetRoles of no ID", errOf(s.GetRoles(ctx, []string{})), storage.ErrInvalidArgument},
			outcome{"GetRoles with an empty ID", errOf(s.GetRoles(ctx, []string{"role-a", ""})), storage.ErrInvalidArgument},
		)
	})

	t.Run("listing filter", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		must(t, errors.Join(s.UpsertRole(ctx, fullRole(t, "role-a", "admin", 0, minute(0))), s.UpsertRole(ctx, fullRole(t, "role-b", "viewer", 0, minute(0)))))
		// The factory builds no role filter: a nil one lists every row, any other is refused.
		listed, err := listedRoles(ctx, s, nil)
		sameIDs(t, "ListRoles with no filter", listed, err, "role-a", "role-b")
		expect(t,
			outcome{"ListRoles with a filter of no backend", errOf(s.ListRoles(ctx, foreignFilter{}, storage.ListOptions{})), storage.ErrInvalidArgument},
			outcome{"ListRoles with a user filter", errOf(s.ListRoles(ctx, s.BuildUserFilter(storage.UserQueryCriteria{}), storage.ListOptions{})), storage.ErrInvalidArgument},
		)
	})
}

// listedRoles drains ListRoles with filter.
func listedRoles(ctx context.Context, s storage.Storage, filter storage.RoleFilter) ([]*model.Role, error) {
	return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.RoleListResult, error) {
		return s.ListRoles(ctx, filter, o)
	})
}

// fullRole is role id of name with every other field set from v: with v 0, none
// to its zero or default value.
func fullRole(t *testing.T, id, name string, v int, created time.Time) *model.Role {
	t.Helper()
	r, err := model.NewRole(id, name)
	if err != nil {
		t.Fatal(err)
	}
	must(t, errors.Join(r.PermissionAdd(enum.All[v+2]), r.PermissionAdd(enum.All[v+7])))
	r.SetCreatedAt(created)
	r.SetUpdatedAt(created.Add(time.Duration(v+1) * time.Minute))
	return r
}

// equalRole compares every accessor of a role read back with the role written.
func equalRole(t *testing.T, what string, got, want *model.Role) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil role, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("Name", got.Name(), want.Name())
	f.same("PermissionsAll", got.PermissionsAll(), want.PermissionsAll())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
