package inmemory

import (
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Compile-time checks that filters implement their respective marker interfaces.
var (
	_ storage.AgentFilter = (*Filter)(nil)
	_ storage.UserFilter  = (*UserFilter)(nil)
	_ storage.RoleFilter  = (*RoleFilter)(nil)
)

// Filter provides predicate-based filtering for in-memory agent queries.
//
// Filters are composed by chaining builder methods, each adding a predicate
// that must be satisfied for an agent to match. All predicates are ANDed together.
type Filter struct {
	predicates []func(*domain.AgentModel) bool
}

// NewFilter creates a new empty filter that matches all agents.
func NewFilter() *Filter {
	return &Filter{
		predicates: make([]func(*domain.AgentModel) bool, 0),
	}
}

// ByPlatform adds a predicate matching agents on the specified platform.
func (f *Filter) ByPlatform(platform string) *Filter {
	f.predicates = append(f.predicates, func(a *domain.AgentModel) bool {
		return a.Platform() == platform
	})
	return f
}

// ByLabel adds a predicate matching agents with a specific label key-value pair.
func (f *Filter) ByLabel(key, value string) *Filter {
	f.predicates = append(f.predicates, func(a *domain.AgentModel) bool {
		v, ok := a.Label(key)
		return ok && v == value
	})
	return f
}

// ByOS adds a predicate matching agents running the specified operating system.
func (f *Filter) ByOS(os string) *Filter {
	f.predicates = append(f.predicates, func(a *domain.AgentModel) bool {
		return a.OS() == os
	})
	return f
}

// ByArch adds a predicate matching agents with the specified architecture.
func (f *Filter) ByArch(arch string) *Filter {
	f.predicates = append(f.predicates, func(a *domain.AgentModel) bool {
		return a.Arch() == arch
	})
	return f
}

// Matches evaluate whether an agent satisfies all predicates in this filter.
//
// Returns true if all predicates pass, false if any predicate fails.
// Empty filters (no predicates) match all agents.
func (f *Filter) Matches(a *domain.AgentModel) bool {
	for _, pred := range f.predicates {
		if !pred(a) {
			return false
		}
	}
	return true
}

// IsAgentFilter implements the storage.AgentFilter marker interface.
func (f *Filter) IsAgentFilter() {}

// UserFilter provides predicate-based filtering for in-memory user queries.
//
// Filters are composed by chaining builder methods, each adding a predicate
// that must be satisfied for a user to match. All predicates are ANDed together.
type UserFilter struct {
	predicates []func(*domain.UserModel) bool
}

// NewUserFilter creates a new empty filter that matches all users.
func NewUserFilter() *UserFilter {
	return &UserFilter{
		predicates: make([]func(*domain.UserModel) bool, 0),
	}
}

// ByEmail adds a predicate matching users with the specified email.
func (f *UserFilter) ByEmail(email string) *UserFilter {
	f.predicates = append(f.predicates, func(u *domain.UserModel) bool {
		return u.Email() == email
	})
	return f
}

// ByDisabled adds a predicate matching users based on their disabled status.
func (f *UserFilter) ByDisabled(disabled bool) *UserFilter {
	f.predicates = append(f.predicates, func(u *domain.UserModel) bool {
		return u.Disabled() == disabled
	})
	return f
}

// ByRoleID adds a predicate matching users who have the specified role id.
func (f *UserFilter) ByRoleID(roleID string) *UserFilter {
	f.predicates = append(f.predicates, func(u *domain.UserModel) bool {
		return u.RoleHas(roleID)
	})
	return f
}

// ByPermission adds a predicate matching users who have the specified direct permission.
func (f *UserFilter) ByPermission(p domain.Permission) *UserFilter {
	f.predicates = append(f.predicates, func(u *domain.UserModel) bool {
		return u.PermissionHas(p)
	})
	return f
}

// Matches evaluate whether a user satisfies all predicates in this filter.
//
// Returns true if all predicates pass, false if any predicate fails.
// Empty filters (no predicates) match all users.
func (f *UserFilter) Matches(u *domain.UserModel) bool {
	for _, pred := range f.predicates {
		if !pred(u) {
			return false
		}
	}
	return true
}

// IsUserFilter implements the storage.UserFilter marker interface.
func (f *UserFilter) IsUserFilter() {}

// RoleFilter provides predicate-based filtering for in-memory role queries.
//
// Filters are composed by chaining builder methods, each adding a predicate
// that must be satisfied for a role to match. All predicates are ANDed together.
type RoleFilter struct {
	predicates []func(*domain.RoleModel) bool
}

// NewRoleFilter creates a new empty filter that matches all roles.
func NewRoleFilter() *RoleFilter {
	return &RoleFilter{
		predicates: make([]func(*domain.RoleModel) bool, 0),
	}
}

// ByName adds a predicate matching roles with the specified name.
func (f *RoleFilter) ByName(name string) *RoleFilter {
	f.predicates = append(f.predicates, func(r *domain.RoleModel) bool {
		return r.Name() == name
	})
	return f
}

// ByPermission adds a predicate matching roles that contain the specified permission.
func (f *RoleFilter) ByPermission(p domain.Permission) *RoleFilter {
	f.predicates = append(f.predicates, func(r *domain.RoleModel) bool {
		return r.PermissionHas(p)
	})
	return f
}

// Matches evaluate whether a role satisfies all predicates in this filter.
func (f *RoleFilter) Matches(r *domain.RoleModel) bool {
	for _, pred := range f.predicates {
		if !pred(r) {
			return false
		}
	}
	return true
}

// IsRoleFilter implements the storage.RoleFilter marker interface.
func (f *RoleFilter) IsRoleFilter() {}
