package apimap

import (
	v1 "github.com/soltiHQ/control-plane/api/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

func User(u *model.User) v1.User {
	if u == nil {
		return v1.User{}
	}

	perms := u.PermissionsAll()
	permStr := make([]string, 0, len(perms))
	for _, p := range perms {
		permStr = append(permStr, string(p))
	}

	return v1.User{
		ID:          u.ID(),
		Subject:     u.Subject(),
		Email:       u.Email(),
		Name:        u.Name(),
		RoleIDs:     u.RoleIDsAll(),
		Permissions: permStr,
		Disabled:    u.Disabled(),
	}
}
