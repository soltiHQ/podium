package dto

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

type RoleDTO struct {
	ID          string
	Name        string
	Permissions []string // kind.Permission as string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func RoleToDTO(r *model.Role) *RoleDTO {
	perms := r.PermissionsAll()
	sp := make([]string, len(perms))
	for i, p := range perms {
		sp[i] = string(p)
	}
	return &RoleDTO{
		ID:          r.ID(),
		Name:        r.Name(),
		Permissions: sp,
		CreatedAt:   r.CreatedAt(),
		UpdatedAt:   r.UpdatedAt(),
	}
}

func RoleFromDTO(d *RoleDTO) (*model.Role, error) {
	if d == nil {
		return nil, nil
	}
	r, err := model.NewRole(d.ID, d.Name)
	if err != nil {
		return nil, err
	}
	for _, p := range kindPermissionsFromStrings(d.Permissions) {
		_ = r.PermissionAdd(p)
	}
	r.SetCreatedAt(d.CreatedAt)
	r.SetUpdatedAt(d.UpdatedAt)
	return r, nil
}
