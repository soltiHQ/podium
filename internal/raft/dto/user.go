package dto

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

type UserDTO struct {
	ID          string
	Subject     string
	Email       string
	Name        string
	Disabled    bool
	RoleIDs     []string
	Permissions []string // kind.Permission as string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func UserToDTO(u *model.User) *UserDTO {
	perms := u.PermissionsAll()
	sp := make([]string, len(perms))
	for i, p := range perms {
		sp[i] = string(p)
	}
	return &UserDTO{
		ID:          u.ID(),
		Subject:     u.Subject(),
		Email:       u.Email(),
		Name:        u.Name(),
		Disabled:    u.Disabled(),
		RoleIDs:     u.RoleIDsAll(),
		Permissions: sp,
		CreatedAt:   u.CreatedAt(),
		UpdatedAt:   u.UpdatedAt(),
	}
}

func UserFromDTO(d *UserDTO) (*model.User, error) {
	if d == nil {
		return nil, nil
	}
	u, err := model.NewUser(d.ID, d.Subject)
	if err != nil {
		return nil, err
	}
	u.SubjectAdd(d.Subject)
	u.EmailAdd(d.Email)
	u.NameAdd(d.Name)
	if d.Disabled {
		u.Disable()
	}
	u.RolesIDsNew(d.RoleIDs)
	u.PermissionsNew(d.Permissions)
	u.SetCreatedAt(d.CreatedAt)
	u.SetUpdatedAt(d.UpdatedAt)
	return u, nil
}
