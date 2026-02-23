package restv1

import "time"

type Session struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`

	AuthKind     string `json:"auth_kind"`
	ID           string `json:"id"`
	UserID       string `json:"user_id"`
	CredentialID string `json:"credential_id"`

	Revoked bool `json:"revoked"`
}

type SessionResponse struct {
	Items []Session `json:"items"`
}
