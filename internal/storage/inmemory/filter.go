package inmemory

import (
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Compile-time checks that filters implement their respective storage interfaces.
var (
	_ storage.AgentFilter = (*AgentFilter)(nil)
	_ storage.UserFilter  = (*UserFilter)(nil)
	_ storage.RoleFilter  = (*RoleFilter)(nil)
)

// AgentFilter provides predicate-based filtering for in-memory agent queries.
//
// Filters are composed by chaining builder methods. All predicates are ANDed together.
// AgentFilter is mutable and not safe for concurrent use.
type AgentFilter struct {
	predicates []func(*model.Agent) bool
}

// NewAgentFilter creates an empty agent filter that matches all agents.
func NewAgentFilter() *AgentFilter {
	return &AgentFilter{predicates: make([]func(*model.Agent) bool, 0)}
}

// ByPlatform matches agents on the specified platform.
func (f *AgentFilter) ByPlatform(platform string) *AgentFilter {
	f.predicates = append(f.predicates, func(a *model.Agent) bool { return a.Platform() == platform })
	return f
}

// ByLabel matches agents that have a label with the given key and value.
func (f *AgentFilter) ByLabel(key, value string) *AgentFilter {
	f.predicates = append(f.predicates, func(a *model.Agent) bool {
		v, ok := a.Label(key)
		return ok && v == value
	})
	return f
}

// ByOS matches agents running the specified operating system.
func (f *AgentFilter) ByOS(os string) *AgentFilter {
	f.predicates = append(f.predicates, func(a *model.Agent) bool { return a.OS() == os })
	return f
}

// ByArch matches agents with the specified CPU architecture.
func (f *AgentFilter) ByArch(arch string) *AgentFilter {
	f.predicates = append(f.predicates, func(a *model.Agent) bool { return a.Arch() == arch })
	return f
}

// Matches reports whether the given agent satisfies all predicates.
func (f *AgentFilter) Matches(a *model.Agent) bool {
	for _, pred := range f.predicates {
		if !pred(a) {
			return false
		}
	}
	return true
}

// IsAgentFilter marks AgentFilter as a valid storage.AgentFilter implementation.
func (f *AgentFilter) IsAgentFilter(storage.AgentFilterSeal) {}

// UserFilter provides predicate-based filtering for in-memory user queries.
//
// Filters are composed by chaining builder methods. All predicates are ANDed together.
// UserFilter is mutable and not safe for concurrent use.
type UserFilter struct {
	predicates []func(*model.User) bool
}

// NewUserFilter creates an empty user filter that matches all users.
func NewUserFilter() *UserFilter {
	return &UserFilter{predicates: make([]func(*model.User) bool, 0)}
}

// ByEmail matches users with the specified email.
func (f *UserFilter) ByEmail(email string) *UserFilter {
	f.predicates = append(f.predicates, func(u *model.User) bool { return u.Email() == email })
	return f
}

// ByDisabled matches users based on their disabled status.
func (f *UserFilter) ByDisabled(disabled bool) *UserFilter {
	f.predicates = append(f.predicates, func(u *model.User) bool { return u.Disabled() == disabled })
	return f
}

// ByRoleID matches users who have the specified role ID assigned.
func (f *UserFilter) ByRoleID(roleID string) *UserFilter {
	f.predicates = append(f.predicates, func(u *model.User) bool { return u.RoleHas(roleID) })
	return f
}

// ByPermission matches users who have the specified direct permission.
func (f *UserFilter) ByPermission(p kind.Permission) *UserFilter {
	f.predicates = append(f.predicates, func(u *model.User) bool { return u.PermissionHas(p) })
	return f
}

// Matches reports whether the given user satisfies all predicates.
func (f *UserFilter) Matches(u *model.User) bool {
	for _, pred := range f.predicates {
		if !pred(u) {
			return false
		}
	}
	return true
}

// IsUserFilter marks UserFilter as a valid storage.UserFilter implementation.
func (f *UserFilter) IsUserFilter(storage.UserFilterSeal) {}

// RoleFilter provides predicate-based filtering for in-memory role queries.
//
// Filters are composed by chaining builder methods. All predicates are ANDed together.
// RoleFilter is mutable and not safe for concurrent use.
type RoleFilter struct {
	predicates []func(*model.Role) bool
}

// NewRoleFilter creates an empty role filter that matches all roles.
func NewRoleFilter() *RoleFilter {
	return &RoleFilter{predicates: make([]func(*model.Role) bool, 0)}
}

// ByName matches roles with the specified name.
func (f *RoleFilter) ByName(name string) *RoleFilter {
	f.predicates = append(f.predicates, func(r *model.Role) bool { return r.Name() == name })
	return f
}

// ByPermission matches roles that contain the specified permission.
func (f *RoleFilter) ByPermission(p kind.Permission) *RoleFilter {
	f.predicates = append(f.predicates, func(r *model.Role) bool { return r.PermissionHas(p) })
	return f
}

// Matches reports whether the given role satisfies all predicates.
func (f *RoleFilter) Matches(r *model.Role) bool {
	for _, pred := range f.predicates {
		if !pred(r) {
			return false
		}
	}
	return true
}

// IsRoleFilter marks RoleFilter as a valid storage.RoleFilter implementation.
func (f *RoleFilter) IsRoleFilter(storage.RoleFilterSeal) {}
