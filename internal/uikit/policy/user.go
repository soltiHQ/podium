package policy

import (
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
)

// UserDetail is a UI-oriented policy for the user detail page.
//
// It controls edit, role-management, delete and session actions on the profile view.
// Self-targeting guards prevent a user from deleting or demoting themselves:
// CanEditRoles and CanDelete are forced to be false when IsSelf is true.
// Another user's actions mirror the API's target ceiling: the viewer must hold
// every permission of that user.
type UserDetail struct {
	IsSelf            bool
	CanEdit           bool
	CanEditRoles      bool
	CanDelete         bool
	CanRevokeSessions bool
}

// BuildUserDetail derives UI action flags from the authenticated identity and
// the target user's effective permissions.
func BuildUserDetail(id *identity.Identity, targetUserID string, targetPerms map[enum.Permission]struct{}) UserDetail {
	if id == nil {
		return UserDetail{}
	}

	var (
		perms = permSet(id)
		self  = id.UserID == targetUserID
	)
	manage := self || covers(perms, targetPerms)
	return UserDetail{
		IsSelf:            self,
		CanEdit:           hasAny(perms, usersEdit) && manage,
		CanEditRoles:      hasAny(perms, usersEdit) && manage && !self,
		CanDelete:         hasAny(perms, usersDelete) && manage && !self,
		CanRevokeSessions: hasAny(perms, usersEdit) && manage,
	}
}
