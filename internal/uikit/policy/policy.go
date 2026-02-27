// Package policy provides lightweight, UI-specific permission models.
//
// Each Build* function takes the current [identity.Identity] and returns a small struct of bool flags
// that templ templates use to show/hide interactive elements (buttons, links, forms).
// This keeps authorization checks out of markup and centralizes them in one place.
package policy

import (
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
)

// Convenience aliases to keep policy builders readable.
const (
	// agents
	agentsGet  = kind.AgentsGet
	agentsEdit = kind.AgentsEdit

	// users
	usersGet    = kind.UsersGet
	usersAdd    = kind.UsersAdd
	usersEdit   = kind.UsersEdit
	usersDelete = kind.UsersDelete

	// specs (task specs)
	specsGet    = kind.SpecsGet
	specsAdd    = kind.SpecsAdd
	specsEdit   = kind.SpecsEdit
	specsDeploy = kind.SpecsDeploy
)

func permSet(id *identity.Identity) map[kind.Permission]struct{} {
	m := make(map[kind.Permission]struct{}, len(id.Permissions))
	for _, p := range id.Permissions {
		m[p] = struct{}{}
	}
	return m
}

func hasAny(set map[kind.Permission]struct{}, wants ...kind.Permission) bool {
	for _, w := range wants {
		if _, ok := set[w]; ok {
			return true
		}
	}
	return false
}
