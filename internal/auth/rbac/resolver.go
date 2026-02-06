package rbac

import (
	"context"
	"sort"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Resolver computes effective permissions for a user.
type Resolver struct {
	store storage.Storage
}

// NewResolver creates a new RBAC resolver.
func NewResolver(store storage.Storage) *Resolver {
	return &Resolver{store: store}
}

// ResolveUserPermissions returns the effective permission set for the given user.
//
// Effective permissions are the union of:
//   - permissions directly assigned to the user
//   - permissions granted by roles assigned to the user
func (r *Resolver) ResolveUserPermissions(ctx context.Context, u *model.User) ([]kind.Permission, error) {
	if r == nil || r.store == nil || u == nil {
		return nil, ErrInvalidArgument
	}

	var (
		userPerms = u.PermissionsAll()
		set       = make(map[kind.Permission]struct{}, len(userPerms)+8)
	)
	for _, p := range userPerms {
		if p != "" {
			set[p] = struct{}{}
		}
	}

	roleIDs := u.RoleIDsAll()
	if len(roleIDs) != 0 {
		roles, err := r.store.GetRoles(ctx, roleIDs)
		if err != nil {
			return nil, err
		}
		for _, role := range roles {
			if role == nil {
				continue
			}
			for _, p := range role.PermissionsAll() {
				if p != "" {
					set[p] = struct{}{}
				}
			}
		}
	}
	if len(set) == 0 {
		return nil, ErrUnauthorized
	}

	out := make([]kind.Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
