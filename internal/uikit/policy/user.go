package policy

import "github.com/soltiHQ/control-plane/internal/auth/identity"

// UserDetail is a UI-oriented policy for the user detail page.
//
// It controls edit, role-management, and delete actions on the profile view.
// Self-targeting guards prevent a user from deleting or demoting themselves:
// CanEditRoles and CanDelete are forced to be false when IsSelf is true.
type UserDetail struct {
	IsSelf       bool
	CanEdit      bool
	CanEditRoles bool
	CanDelete    bool
}

// BuildUserDetail derives UI action flags from the authenticated identity.
func BuildUserDetail(id *identity.Identity, targetUserID string) UserDetail {
	if id == nil {
		return UserDetail{}
	}

	var (
		perms = permSet(id)
		self  = id.UserID == targetUserID
	)
	return UserDetail{
		IsSelf:       self,
		CanEdit:      hasAny(perms, usersEdit),
		CanEditRoles: hasAny(perms, usersEdit) && !self,
		CanDelete:    hasAny(perms, usersDelete) && !self,
	}
}
