package restv1

type Credential struct {
	Auth   string `json:"auth"`
	ID     string `json:"id"`
	UserID string `json:"user_id"`
}

type CredentialListResponse struct {
	Items []Credential `json:"items"`
}

// SetPasswordRequest describes a password change request.
type SetPasswordRequest struct {
	Password string `json:"password"`
}
