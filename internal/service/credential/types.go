package credential

import "github.com/soltiHQ/control-plane/domain/model"

const defaultListLimit = 1000

// ListByUserQuery describes listing credentials for a user.
type ListByUserQuery struct {
	Limit  int
	UserID string
}

// Page is a list result.
type Page struct {
	Items []*model.Credential
}

// DeleteRequest describes a credential deletion request.
type DeleteRequest struct {
	ID string
}

// SetPasswordRequest sets/replaces password verification material for a user.
type SetPasswordRequest struct {
	Cost int

	Password     string
	VerifierID   string
	CredentialID string
	UserID       string
}
