package policy

import "github.com/soltiHQ/control-plane/internal/auth/identity"

// AgentDetail is a UI-oriented policy for the agent detail page.
//
// It controls which interactive elements (buttons, forms) are rendered in the agent detail view based on the caller's permissions.
//
// Passed into the templ component, markup stays free of auth logic.
type AgentDetail struct {
	CanEditLabels   bool
	CanOperateTasks bool
	CanViewHistory  bool

	// CanResetCredential shows the credential reset and CanDelete the delete of
	// the agent, each with its own permission (agents:reset, agents:delete): the
	// API requires the same, and agents:edit gives neither.
	CanResetCredential bool
	CanDelete          bool
}

// BuildAgentDetail derives UI action flags from the authenticated identity.
func BuildAgentDetail(id *identity.Identity) AgentDetail {
	if id == nil {
		return AgentDetail{}
	}

	perms := permSet(id)
	return AgentDetail{
		CanEditLabels:   hasAny(perms, agentsEdit),
		CanOperateTasks: hasAny(perms, agentsEdit),
		CanViewHistory:  hasAny(perms, agentsGet),

		CanResetCredential: hasAny(perms, agentsReset),
		CanDelete:          hasAny(perms, agentsDelete),
	}
}
