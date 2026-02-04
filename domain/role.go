package domain

import "time"

var _ Entity[*RoleModel] = (*RoleModel)(nil)

// RoleModel describes a named set of permissions.
type RoleModel struct {
	updatedAt time.Time

	name string
	id   string

	permissions []Permission
}

// NewRoleModel creates a new role model.
func NewRoleModel(id, name string) (*RoleModel, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	if name == "" {
		return nil, ErrEmptyName
	}
	role := &RoleModel{
		updatedAt:   time.Now(),
		id:          id,
		name:        name,
		permissions: make([]Permission, 0),
	}
	return role, nil
}

// ID returns the unique identifier for this role.
func (r *RoleModel) ID() string {
	return r.id
}

// Name returns the role name.
func (r *RoleModel) Name() string {
	return r.name
}

// UpdatedAt returns the last modification timestamp.
func (r *RoleModel) UpdatedAt() time.Time {
	return r.updatedAt
}

// PermissionsAll returns a copy of the role permissions.
func (r *RoleModel) PermissionsAll() []Permission {
	out := make([]Permission, 0, len(r.permissions))
	for _, p := range r.permissions {
		out = append(out, p)
	}
	return out
}

// PermissionHas checks whether the role has the given permission.
func (r *RoleModel) PermissionHas(p Permission) bool {
	for _, x := range r.permissions {
		if x == p {
			return true
		}
	}
	return false
}

// PermissionAdd grants a permission to the role.
func (r *RoleModel) PermissionAdd(p Permission) error {
	if p == "" {
		return ErrEmptyID
	}
	if r.PermissionHas(p) {
		return nil
	}
	r.permissions = append(r.permissions, p)
	r.updatedAt = time.Now()
	return nil
}

// PermissionDelete revokes a permission from the role.
func (r *RoleModel) PermissionDelete(p Permission) {
	for i, x := range r.permissions {
		if x == p {
			r.permissions = append(r.permissions[:i], r.permissions[i+1:]...)
			r.updatedAt = time.Now()
			return
		}
	}
}

// Rename changes the role name.
func (r *RoleModel) Rename(name string) error {
	if name == "" {
		return ErrEmptyID
	}
	if r.name == name {
		return nil
	}
	r.name = name
	r.updatedAt = time.Now()
	return nil
}

// Clone creates a deep copy of the role model.
func (r *RoleModel) Clone() *RoleModel {
	out := make([]Permission, 0, len(r.permissions))
	for _, p := range r.permissions {
		out = append(out, p)
	}
	return &RoleModel{
		id:          r.id,
		name:        r.name,
		updatedAt:   r.updatedAt,
		permissions: out,
	}
}
