package policy

import (
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
)

// Dashboard is a UI-oriented policy for the dashboard issue list.
type Dashboard struct {
	CanEditAgents bool
	CanEditSpecs  bool
	CanEditUsers  bool
}

// BuildDashboard derives UI action flags from the authenticated identity.
func BuildDashboard(id *identity.Identity) Dashboard {
	if id == nil {
		return Dashboard{}
	}

	perms := permSet(id)
	return Dashboard{
		CanEditAgents: hasAny(perms, agentsEdit),
		CanEditSpecs:  hasAny(perms, specsEdit),
		CanEditUsers:  hasAny(perms, usersEdit),
	}
}

// CanDismiss reports whether the viewer may close issues of the given event
// kind: the API requires the edit permission of the issue's subject, and all
// three for a kind without a single subject.
func (d Dashboard) CanDismiss(kind string) bool {
	switch event.SubjectOf(kind) {
	case event.SubjectAgent:
		return d.CanEditAgents
	case event.SubjectSpec:
		return d.CanEditSpecs
	case event.SubjectUser:
		return d.CanEditUsers
	default:
		return d.CanEditAgents && d.CanEditSpecs && d.CanEditUsers
	}
}
