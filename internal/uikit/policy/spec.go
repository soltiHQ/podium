package policy

import "github.com/soltiHQ/control-plane/internal/auth/identity"

// SpecDetail is a UI-oriented policy for the task spec detail page.
//
// It governs edit/deploy/delete actions shown on the detail view.
// CanDelete reuses the specsEdit permission — there is no separate "delete" permission for task specs at the domain level.
type SpecDetail struct {
	CanEdit   bool
	CanDeploy bool
	CanDelete bool
}

// BuildSpecDetail derives UI action flags from the authenticated identity.
func BuildSpecDetail(id *identity.Identity) SpecDetail {
	if id == nil {
		return SpecDetail{}
	}

	perms := permSet(id)
	return SpecDetail{
		CanEdit:   hasAny(perms, specsEdit),
		CanDeploy: hasAny(perms, specsDeploy),
		CanDelete: hasAny(perms, specsEdit),
	}
}
