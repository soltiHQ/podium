package rbac

import (
	"context"
	"sort"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Resolver computes effective permissions for a user by combining:
//
//   - direct per-user permissions, and
//   - permissions granted via the user's roles.
//
// The resolver does not perform authentication, token issuance, or policy decisions.
// It only derives an effective permission set from stored assignments.
type Resolver struct {
	store storage.Storage
}

// NewResolver creates a new RBAC resolver bound to the provided storage backend.
func NewResolver(store storage.Storage) *Resolver {
	return &Resolver{store: store}
}

// ResolveUserPermissions returns the effective permission set for the given user.
//
// Contract:
//   - The result is a de-duplicated union of user direct permissions and role permissions.
//   - Empty permissions are ignored.
//   - The returned slice is sorted ascending to provide deterministic output.
//   - The method treats the provided user and returned domain entities as read-only.
//
// Errors:
//   - auth.ErrInvalidArgument if resolver/store/user is nil.
//   - Propagates storage errors from role lookup (e.g., ErrInvalidArgument, ErrNotFound,
//     ErrUnavailable, ErrInternal), without wrapping them into auth-level errors.
func (r *Resolver) ResolveUserPermissions(ctx context.Context, u *model.User) ([]enum.Permission, error) {
	if r == nil || r.store == nil || u == nil {
		return nil, auth.ErrInvalidArgument
	}

	var (
		userPerms = u.PermissionsAll()
		set       = make(map[enum.Permission]struct{}, len(userPerms)+8)
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
		return []enum.Permission{}, nil
	}

	out := make([]enum.Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
