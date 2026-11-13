package inmemory

import (
	"slices"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// AgentFilter provides predicate-based filtering for in-memory agent queries.
//
// Filters are composed by chaining builder methods. All predicates are ANDed together.
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

// Query matches agents by id/name/endpoint (case-insensitive substring).
func (f *AgentFilter) Query(q string) *AgentFilter {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return f
	}
	f.predicates = append(f.predicates, func(a *model.Agent) bool {
		if a == nil {
			return false
		}
		if strings.Contains(strings.ToLower(a.ID()), q) {
			return true
		}
		if strings.Contains(strings.ToLower(a.Name()), q) {
			return true
		}
		if strings.Contains(strings.ToLower(a.Endpoint()), q) {
			return true
		}
		return false
	})
	return f
}

// StaleAtBefore matches agents whose staleness deadline has passed or was never set.
// Zero staleAt is treated as "always eligible" so agents without a computed
// deadline (e.g., heartbeatInterval=0) are not silently dropped.
func (f *AgentFilter) StaleAtBefore(t time.Time) *AgentFilter {
	f.predicates = append(f.predicates, func(a *model.Agent) bool {
		return a.StaleAt().IsZero() || a.StaleAt().Before(t)
	})
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

// agentPredicate resolves the filter of a listing or a scan to its predicate.
// A nil filter accepts every agent; one built by another backend, or a nil *AgentFilter,
// is rejected.
func agentPredicate(filter storage.AgentFilter) (func(*model.Agent) bool, error) {
	if filter == nil {
		return nil, nil
	}
	f, ok := filter.(*AgentFilter)
	if !ok || f == nil {
		return nil, storage.ErrInvalidArgument
	}
	return f.Matches, nil
}

// userPredicate resolves the filter of a listing to its predicate. A nil filter
// accepts every user; one built by another backend, or a nil *UserFilter, is
// rejected.
func userPredicate(filter storage.UserFilter) (func(*model.User) bool, error) {
	if filter == nil {
		return nil, nil
	}
	f, ok := filter.(*UserFilter)
	if !ok || f == nil {
		return nil, storage.ErrInvalidArgument
	}
	return f.Matches, nil
}

// UserFilter provides predicate-based filtering for in-memory user queries.
//
// Filters are composed by chaining builder methods. All predicates are ANDed together.
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
func (f *UserFilter) ByPermission(p enum.Permission) *UserFilter {
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

// Query matches users by subject/name/email (case-insensitive substring).
func (f *UserFilter) Query(q string) *UserFilter {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return f
	}
	f.predicates = append(f.predicates, func(u *model.User) bool {
		if u == nil {
			return false
		}
		if strings.Contains(strings.ToLower(u.Subject()), q) {
			return true
		}
		if strings.Contains(strings.ToLower(u.Name()), q) {
			return true
		}
		if strings.Contains(strings.ToLower(u.Email()), q) {
			return true
		}
		return false
	})
	return f
}

// RoleFilter provides predicate-based filtering for in-memory role queries.
//
// Filters are composed by chaining builder methods. All predicates are ANDed together.
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
func (f *RoleFilter) ByPermission(p enum.Permission) *RoleFilter {
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

// rolePredicate resolves the filter of a listing to its predicate. A nil filter
// accepts every role; one built by another backend, or a nil *RoleFilter, is
// rejected.
func rolePredicate(filter storage.RoleFilter) (func(*model.Role) bool, error) {
	if filter == nil {
		return nil, nil
	}
	f, ok := filter.(*RoleFilter)
	if !ok || f == nil {
		return nil, storage.ErrInvalidArgument
	}
	return f.Matches, nil
}

// SpecFilter provides predicate-based filtering for in-memory task spec queries.
type SpecFilter struct {
	predicates []func(*model.Spec) bool
}

// NewSpecFilter creates an empty task spec filter that matches all task specs.
func NewSpecFilter() *SpecFilter {
	return &SpecFilter{predicates: make([]func(*model.Spec) bool, 0)}
}

// Query matches task specs by name/slot (case-insensitive substring).
func (f *SpecFilter) Query(q string) *SpecFilter {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return f
	}
	f.predicates = append(f.predicates, func(ts *model.Spec) bool {
		if ts == nil {
			return false
		}
		if strings.Contains(strings.ToLower(ts.Name()), q) {
			return true
		}
		if strings.Contains(strings.ToLower(ts.Slot()), q) {
			return true
		}
		return false
	})
	return f
}

// Matches reports whether the given task spec satisfies all predicates.
func (f *SpecFilter) Matches(ts *model.Spec) bool {
	for _, pred := range f.predicates {
		if !pred(ts) {
			return false
		}
	}
	return true
}

// specPredicate resolves the filter of a listing or a scan to its predicate.
// A nil filter accepts every spec; one built by another backend, or a nil *SpecFilter,
// is rejected.
func specPredicate(filter storage.SpecFilter) (func(*model.Spec) bool, error) {
	if filter == nil {
		return nil, nil
	}
	f, ok := filter.(*SpecFilter)
	if !ok || f == nil {
		return nil, storage.ErrInvalidArgument
	}
	return f.Matches, nil
}

// RolloutFilter provides predicate-based filtering for in-memory rollout queries.
//
// Filters are composed by chaining builder methods. All predicates are ANDed together.
type RolloutFilter struct {
	predicates []func(*model.Rollout) bool
}

// NewRolloutFilter creates an empty rollout filter that matches all rollouts.
func NewRolloutFilter() *RolloutFilter {
	return &RolloutFilter{predicates: make([]func(*model.Rollout) bool, 0)}
}

// BySpecID matches rollouts for a given spec.
func (f *RolloutFilter) BySpecID(id string) *RolloutFilter {
	f.predicates = append(f.predicates, func(ss *model.Rollout) bool { return ss.SpecID() == id })
	return f
}

// ByAgentID matches rollouts for a given agent.
func (f *RolloutFilter) ByAgentID(id string) *RolloutFilter {
	f.predicates = append(f.predicates, func(ss *model.Rollout) bool { return ss.AgentID() == id })
	return f
}

// ByStatus matches rollouts with a given sync status.
func (f *RolloutFilter) ByStatus(s enum.SyncStatus) *RolloutFilter {
	f.predicates = append(f.predicates, func(ss *model.Rollout) bool { return ss.Status() == s })
	return f
}

// ByStatuses matches rollouts whose status is any of the provided values (OR).
// The filter keeps its own copy of statuses: a caller that changes its slice
// later does not change the filter.
func (f *RolloutFilter) ByStatuses(statuses ...enum.SyncStatus) *RolloutFilter {
	statuses = slices.Clone(statuses)
	f.predicates = append(f.predicates, func(ss *model.Rollout) bool {
		for _, s := range statuses {
			if ss.Status() == s {
				return true
			}
		}
		return false
	})
	return f
}

// Matches reports whether the given rollout satisfies all predicates.
func (f *RolloutFilter) Matches(ss *model.Rollout) bool {
	for _, pred := range f.predicates {
		if !pred(ss) {
			return false
		}
	}
	return true
}

// rolloutPredicate resolves the filter of a listing or a scan to its predicate.
// A nil filter accepts every rollout; one built by another backend, or a nil *RolloutFilter,
// is rejected.
func rolloutPredicate(filter storage.RolloutFilter) (func(*model.Rollout) bool, error) {
	if filter == nil {
		return nil, nil
	}
	f, ok := filter.(*RolloutFilter)
	if !ok || f == nil {
		return nil, storage.ErrInvalidArgument
	}
	return f.Matches, nil
}
