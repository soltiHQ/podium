package apimap

import (
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

func User(u *model.User) restv1.User {
	if u == nil {
		return restv1.User{}
	}
	var (
		perms   = u.PermissionsAll()
		permStr = make([]string, 0, len(perms))
	)
	for _, p := range perms {
		permStr = append(permStr, string(p))
	}
	return restv1.User{
		RoleIDs:     u.RoleIDsAll(),
		Disabled:    u.Disabled(),
		Subject:     u.Subject(),
		Email:       u.Email(),
		Name:        u.Name(),
		ID:          u.ID(),
		Permissions: permStr,
	}
}
