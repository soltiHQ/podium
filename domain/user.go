package domain

import "time"

var _ Entity[*UserModel] = (*UserModel)(nil)

// UserModel represents a system user (human or service).
type UserModel struct {
	updatedAt time.Time

	subject  string
	email    string
	name     string
	id       string
	disabled bool

	roleIDs     []string
	permissions []Permission
}

// NewUserModel creates a new user domain model.
func NewUserModel(id, subject string) (*UserModel, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	if subject == "" {
		return nil, ErrInvalidSubject
	}
	return &UserModel{
		id:          id,
		subject:     subject,
		updatedAt:   time.Now(),
		roleIDs:     make([]string, 0),
		permissions: make([]Permission, 0),
	}, nil
}

// ID returns unique user identifier.
func (u *UserModel) ID() string {
	return u.id
}

// Subject returns stable auth subject (JWT sub).
func (u *UserModel) Subject() string {
	return u.subject
}

// Email returns user email.
func (u *UserModel) Email() string {
	return u.email
}

// Name returns display name.
func (u *UserModel) Name() string {
	return u.name
}

// Disabled reports whether user is disabled.
func (u *UserModel) Disabled() bool {
	return u.disabled
}

// UpdatedAt returns last modification time.
func (u *UserModel) UpdatedAt() time.Time {
	return u.updatedAt
}

// RoleIDsAll returns assigned role IDs.
func (u *UserModel) RoleIDsAll() []string {
	out := make([]string, 0, len(u.roleIDs))
	for _, id := range u.roleIDs {
		out = append(out, id)
	}
	return out
}

// PermissionsAll returns per-user permissions.
func (u *UserModel) PermissionsAll() []Permission {
	out := make([]Permission, 0, len(u.permissions))
	for _, p := range u.permissions {
		out = append(out, p)
	}
	return out
}

// RoleAdd assigns a role to the user.
func (u *UserModel) RoleAdd(roleID string) error {
	if roleID == "" {
		return ErrEmptyID
	}
	for _, id := range u.roleIDs {
		if id == roleID {
			return nil
		}
	}
	u.roleIDs = append(u.roleIDs, roleID)
	u.updatedAt = time.Now()
	return nil
}

// RoleDelete removes a role from the user.
func (u *UserModel) RoleDelete(roleID string) {
	for i, id := range u.roleIDs {
		if id == roleID {
			u.roleIDs = append(u.roleIDs[:i], u.roleIDs[i+1:]...)
			u.updatedAt = time.Now()
			return
		}
	}
}

// PermissionAdd grants a permission directly to the user.
func (u *UserModel) PermissionAdd(p Permission) error {
	if p == "" {
		return ErrEmptyID
	}
	for _, x := range u.permissions {
		if x == p {
			return nil
		}
	}
	u.permissions = append(u.permissions, p)
	u.updatedAt = time.Now()
	return nil
}

// PermissionDelete revokes a permission from the user.
func (u *UserModel) PermissionDelete(p Permission) {
	for i, x := range u.permissions {
		if x == p {
			u.permissions = append(u.permissions[:i], u.permissions[i+1:]...)
			u.updatedAt = time.Now()
			return
		}
	}
}

// RoleHas checks whether a user has the given role id.
func (u *UserModel) RoleHas(roleID string) bool {
	for _, id := range u.roleIDs {
		if id == roleID {
			return true
		}
	}
	return false
}

// PermissionHas checks whether a user has a directly granted permission.
func (u *UserModel) PermissionHas(p Permission) bool {
	for _, x := range u.permissions {
		if x == p {
			return true
		}
	}
	return false
}

// Clone creates a deep copy.
func (u *UserModel) Clone() *UserModel {
	roleIDs := make([]string, 0, len(u.roleIDs))
	for _, id := range u.roleIDs {
		roleIDs = append(roleIDs, id)
	}
	perms := make([]Permission, 0, len(u.permissions))
	for _, p := range u.permissions {
		perms = append(perms, p)
	}
	return &UserModel{
		id:          u.id,
		subject:     u.subject,
		email:       u.email,
		name:        u.name,
		disabled:    u.disabled,
		updatedAt:   u.updatedAt,
		roleIDs:     roleIDs,
		permissions: perms,
	}
}
