package user

import (
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/internal/ui/routepath"
	"github.com/soltiHQ/control-plane/ui/templates/component/modal"
)

func createFields() []modal.Field {
	return []modal.Field{
		{ID: "subject", Label: "Subject", Placeholder: "Username", Required: true},
		{ID: "name", Label: "Name", Placeholder: "Full name"},
		{ID: "email", Label: "Email", Placeholder: "Email address"},
	}
}

func editFields(u restv1.User) []modal.Field {
	return []modal.Field{
		{ID: "subject", Label: "Subject", Value: u.Subject, Placeholder: "Username", Required: true},
		{ID: "name", Label: "Name", Value: u.Name, Placeholder: "Full name"},
		{ID: "email", Label: "Email", Value: u.Email, Placeholder: "Email address"},
	}
}

func editSelects(u restv1.User) []modal.AsyncSelect {
	return []modal.AsyncSelect{
		{
			ID:       "role_ids",
			Label:    "Roles",
			Endpoint: routepath.ApiRoles,
			Selected: u.RoleIDs,
			ValueKey: "id",
			LabelKey: "name",
		},
		{
			ID:       "permissions",
			Label:    "Permissions",
			Endpoint: routepath.ApiPermissions,
			Selected: u.Permissions,
		},
	}
}
