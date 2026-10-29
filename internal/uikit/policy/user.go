package policy

import "github.com/soltiHQ/control-plane/internal/auth/identity"

// UserDetail is a UI-oriented policy for the user detail page.
//
// It controls edit, role-management, delete and session actions on the profile view.
// Self-targeting guards prevent a user from deleting or demoting themselves:
// CanEditRoles and CanDelete are forced to be false when IsSelf is true.
type UserDetail struct {
	IsSelf            bool
	CanEdit           bool
	CanEditRoles      bool
	CanDelete         bool
	CanRevokeSessions bool
}

// BuildUserDetail derives UI action flags from the authenticated identity.
// manageable is the API's target-ceiling decision for this viewer and user
// (user.Service.CanManage); it is always true for the viewer's own account.
func BuildUserDetail(id *identity.Identity, targetUserID string, manageable bool) UserDetail {
	if id == nil {
		return UserDetail{}
	}

	var (
		perms = permSet(id)
		self  = id.UserID == targetUserID
	)
	return UserDetail{
		IsSelf:            self,
		CanEdit:           hasAny(perms, usersEdit) && manageable,
		CanEditRoles:      hasAny(perms, usersEdit) && manageable && !self,
		CanDelete:         hasAny(perms, usersDelete) && manageable && !self,
		CanRevokeSessions: hasAny(perms, usersEdit) && manageable,
	}
}
