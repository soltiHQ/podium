package user

import (
	"context"
	"errors"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/storage"
)

var (
	// ErrPermissionCeiling reports an operation that would grant a permission
	// the actor does not hold, or act on another user who holds one.
	ErrPermissionCeiling = errors.New("operation exceeds the actor's permissions")

	// ErrLastAdmin reports an operation that would leave no enabled user
	// holding every permission.
	ErrLastAdmin = errors.New("at least one enabled user must keep all permissions")
)

// EffectivePermissions returns the union of u's role and direct permissions.
// It uses the resolver that authorizes requests, so a ceiling and the
// permissions a request runs with always agree.
func EffectivePermissions(ctx context.Context, tx storage.Storage, u *model.User) (map[enum.Permission]struct{}, error) {
	perms, err := rbac.NewResolver(tx).ResolveUserPermissions(ctx, u)
	if err != nil {
		return nil, err
	}
	set := make(map[enum.Permission]struct{}, len(perms))
	for _, p := range perms {
		set[p] = struct{}{}
	}
	return set, nil
}

// CheckTarget returns ErrPermissionCeiling unless actor may manage target.
// Acting on yourself is within the ceiling. Acting on another user requires
// the actor to hold every effective permission of that user. The actor's
// permissions are the ones request authorization resolved for the identity.
// A nil target (such as the owner of an orphaned session) holds none.
func CheckTarget(ctx context.Context, tx storage.Storage, actor *identity.Identity, target *model.User) error {
	if actor == nil {
		return ErrPermissionCeiling
	}
	if target == nil || target.ID() == actor.UserID {
		return nil
	}
	return checkGrant(ctx, tx, actor, target)
}

// checkGrant returns ErrPermissionCeiling unless actor holds every effective
// permission of u.
func checkGrant(ctx context.Context, tx storage.Storage, actor *identity.Identity, u *model.User) error {
	if actor == nil {
		return ErrPermissionCeiling
	}
	perms, err := EffectivePermissions(ctx, tx, u)
	if err != nil {
		return err
	}
	for p := range perms {
		if !actor.HasPermission(p) {
			return ErrPermissionCeiling
		}
	}
	return nil
}

// keepAdmin returns ErrLastAdmin when changing before into after (nil: deleted)
// takes away the last enabled user holding every permission. Call it before
// the transaction writes: a raft transaction's listings do not show its own
// pending writes.
func keepAdmin(ctx context.Context, tx storage.Storage, before, after *model.User) error {
	if was, err := isAdmin(ctx, tx, before); err != nil || !was {
		return err
	}
	if still, err := isAdmin(ctx, tx, after); err != nil || still {
		return err
	}
	users, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.UserListResult, error) {
		return tx.ListUsers(ctx, nil, o)
	})
	if err != nil {
		return err
	}
	for _, u := range users {
		if u == nil || u.ID() == before.ID() {
			continue
		}
		if ok, err := isAdmin(ctx, tx, u); err != nil || ok {
			return err
		}
	}
	return ErrLastAdmin
}

// isAdmin reports whether u is enabled and holds every declared permission.
func isAdmin(ctx context.Context, tx storage.Storage, u *model.User) (bool, error) {
	if u == nil || u.Disabled() {
		return false, nil
	}
	perms, err := EffectivePermissions(ctx, tx, u)
	if err != nil {
		return false, err
	}
	for _, p := range enum.All {
		if _, ok := perms[p]; !ok {
			return false, nil
		}
	}
	return true, nil
}
