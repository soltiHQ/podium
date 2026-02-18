package user

import (
	v1 "github.com/soltiHQ/control-plane/api/v1"
	"github.com/soltiHQ/control-plane/ui/templates/component/modal"
)

func editFields(u v1.User) []modal.Field {
	return []modal.Field{
		{ID: "subject", Label: "Subject", Value: u.Subject, Placeholder: "Username", Required: true},
		{ID: "name", Label: "Name", Value: u.Name, Placeholder: "Full name"},
		{ID: "email", Label: "Email", Value: u.Email, Placeholder: "Email address"},
	}
}
