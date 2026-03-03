package apimapv1

import (
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
)

// roleNameIndex maps role ID → display name (built once at init).
var roleNameIndex = func() map[string]string {
	m := make(map[string]string, len(kind.BuiltinRoles))
	for _, r := range kind.BuiltinRoles {
		m[r.ID] = r.Name
	}
	return m
}()

// User maps a domain User to its REST DTO.
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

	roleIDs := u.RoleIDsAll()
	roleNames := make([]string, 0, len(roleIDs))
	for _, id := range roleIDs {
		if name, ok := roleNameIndex[id]; ok {
			roleNames = append(roleNames, name)
		} else {
			roleNames = append(roleNames, id)
		}
	}

	return restv1.User{
		RoleIDs:     roleIDs,
		RoleNames:   roleNames,
		Disabled:    u.Disabled(),
		Subject:     u.Subject(),
		Email:       u.Email(),
		Name:        u.Name(),
		ID:          u.ID(),
		Permissions: permStr,
	}
}
