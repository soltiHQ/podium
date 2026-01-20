package inmemory

import (
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Compile-time check that Filter implements storage.AgentFilter.
var _ storage.AgentFilter = (*Filter)(nil)

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
