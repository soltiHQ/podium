package restv1

// Credential is the REST representation of an authentication credential.
type Credential struct {
	ID     string `json:"id"`
	Auth   string `json:"auth"`
	UserID string `json:"user_id"`
}

// CredentialListResponse is the list of credentials.
type CredentialListResponse struct {
	Items []Credential `json:"items"`
}

// SetPasswordRequest describes a password change request.
type SetPasswordRequest struct {
	Password string `json:"password"`
}
