package user

import (
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

const defaultListLimit = 60

// ListQuery describes a paginated users listing request.
type ListQuery struct {
	// Filter is a storage-level filter.
	// Backends validate that the filter was constructed for that backend and return storage.ErrInvalidArgument otherwise.
	Filter storage.UserFilter

	Cursor string
	Limit  int
}

// Page is a paginated users listing result.
type Page struct {
	Items      []*model.User
	NextCursor string
}

// UpdatePatch is a partial change to a stored user: a nil field keeps the
// stored value, a set one replaces it. Disabled is not part of it (see
// SetDisabled).
type UpdatePatch struct {
	// Name, Email and Subject are normalized like on Create.
	Name, Email, Subject *string

	// RoleIDs and Permissions replace the whole set, so a pointer to an empty
	// list clears it.
	RoleIDs, Permissions *[]string
}

// apply sets the fields p names on u.
func (p UpdatePatch) apply(u *model.User) {
	if p.Name != nil {
		u.SetName(*p.Name)
	}
	if p.Email != nil {
		u.SetEmail(*p.Email)
	}
	if p.Subject != nil {
		u.SetSubject(*p.Subject)
	}
	if p.RoleIDs != nil {
		u.SetRoleIDs(*p.RoleIDs)
	}
	if p.Permissions != nil {
		u.SetPermissions(*p.Permissions)
	}
}
