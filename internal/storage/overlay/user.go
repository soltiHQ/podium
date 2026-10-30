package overlay

import (
	"context"
	"fmt"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Users and roles have no scan, so their listings drain the base's listing: one
// page of it for up to storage.MaxListLimit rows.

func (t *Tx) UpsertUser(_ context.Context, u *model.User) error {
	if u == nil {
		return storage.ErrInvalidArgument
	}
	msg := wire.UserToProto(u)
	row, err := wire.UserFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.users.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_UserUpsert{UserUpsert: msg}})
	return nil
}

func (t *Tx) GetUser(ctx context.Context, id string) (*model.User, error) {
	return t.users.get(ctx, id, t.base.GetUser)
}

func (t *Tx) GetUserBySubject(ctx context.Context, subject string) (*model.User, error) {
	if subject == "" {
		return nil, storage.ErrInvalidArgument
	}
	return unique(&t.users,
		func(u *model.User) bool { return u.Subject() == subject },
		func() (*model.User, error) { return t.base.GetUserBySubject(ctx, subject) },
		fmt.Sprintf("user subject %q", subject))
}

func (t *Tx) ListUsers(ctx context.Context, filter storage.UserFilter, opts storage.ListOptions) (*storage.UserListResult, error) {
	return list(opts, func() ([]*model.User, error) {
		return scan(&t.users, filter, func() ([]*model.User, error) {
			return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.UserListResult, error) {
				return t.base.ListUsers(ctx, filter, o)
			})
		})
	})
}

func (t *Tx) DeleteUser(ctx context.Context, id string) error {
	if err := t.users.remove(ctx, id, t.base.GetUser); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_UserDelete{UserDelete: id}})
	return nil
}

func (t *Tx) UpsertRole(_ context.Context, r *model.Role) error {
	if r == nil {
		return storage.ErrInvalidArgument
	}
	msg := wire.RoleToProto(r)
	row, err := wire.RoleFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.roles.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_RoleUpsert{RoleUpsert: msg}})
	return nil
}

func (t *Tx) GetRole(ctx context.Context, id string) (*model.Role, error) {
	return t.roles.get(ctx, id, t.base.GetRole)
}

// GetRoles returns the roles in the order of ids, a clone for each ID however often
// it repeats, and ErrNotFound if the transaction does not see one of them. The roles
// it did not write come from one GetRoles of the base.
func (t *Tx) GetRoles(ctx context.Context, ids []string) ([]*model.Role, error) {
	if len(ids) == 0 {
		return nil, storage.ErrInvalidArgument
	}
	for _, id := range ids {
		if id == "" {
			return nil, storage.ErrInvalidArgument
		}
	}
	var (
		found  = make(map[string]*model.Role, len(ids))
		inBase []string
	)
	for _, id := range ids {
		if _, ok := found[id]; ok {
			continue
		}
		if !t.roles.written(id) {
			found[id] = nil
			inBase = append(inBase, id)
			continue
		}
		r, err := t.roles.get(ctx, id, t.base.GetRole)
		if err != nil {
			return nil, err
		}
		found[id] = r
	}
	if len(inBase) > 0 {
		roles, err := t.base.GetRoles(ctx, inBase)
		if err != nil {
			return nil, err
		}
		for _, r := range roles {
			found[r.ID()] = r
		}
	}
	out := make([]*model.Role, 0, len(ids))
	for _, id := range ids {
		r := found[id]
		if r == nil {
			// Should not happen if the base's GetRoles succeeded, but keep it strict.
			return nil, storage.ErrInternal
		}
		out = append(out, r.Clone())
	}
	return out, nil
}

func (t *Tx) GetRoleByName(ctx context.Context, name string) (*model.Role, error) {
	if name == "" {
		return nil, storage.ErrInvalidArgument
	}
	return unique(&t.roles,
		func(r *model.Role) bool { return r.Name() == name },
		func() (*model.Role, error) { return t.base.GetRoleByName(ctx, name) },
		fmt.Sprintf("role name %q", name))
}

func (t *Tx) ListRoles(ctx context.Context, filter storage.RoleFilter, opts storage.ListOptions) (*storage.RoleListResult, error) {
	return list(opts, func() ([]*model.Role, error) {
		return scan(&t.roles, filter, func() ([]*model.Role, error) {
			return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.RoleListResult, error) {
				return t.base.ListRoles(ctx, filter, o)
			})
		})
	})
}

func (t *Tx) DeleteRole(ctx context.Context, id string) error {
	if err := t.roles.remove(ctx, id, t.base.GetRole); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_RoleDelete{RoleDelete: id}})
	return nil
}
