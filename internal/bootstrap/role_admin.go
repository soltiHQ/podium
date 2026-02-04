package bootstrap

import (
	"context"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/storage"

	"github.com/rs/zerolog"
)

const (
	AdminRoleID   = "00000000-0000-0000-0000-000000000010"
	AdminRoleName = "admin"
)

type EnsureAdminRoleStep struct{}

// Name of the step.
func (EnsureAdminRoleStep) Name() string {
	return "ensure_admin_role"
}

// Run a step process.
func (EnsureAdminRoleStep) Run(ctx context.Context, logger zerolog.Logger, store storage.Storage) error {
	if store == nil {
		return storage.ErrInvalidArgument
	}

	_, err := store.GetRole(ctx, AdminRoleID)
	if err == nil {
		return nil
	}
	role, err := domain.NewRoleModel(AdminRoleID, AdminRoleName)
	if err != nil {
		return err
	}
	for _, p := range domain.PermissionsAll {
		_ = role.PermissionAdd(p)
	}
	if err = store.UpsertRole(ctx, role); err != nil {
		return err
	}

	logger.Info().
		Str("role_id", AdminRoleID).
		Str("role_name", AdminRoleName).
		Msg("BOOTSTRAP: Created admin role")
	return nil
}
