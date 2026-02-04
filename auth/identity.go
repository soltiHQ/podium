package auth

import "time"

// Identity describes an authenticated principal (user/service).
type Identity struct {
	ExpiresAt time.Time
	NotBefore time.Time
	IssuedAt  time.Time

	Subject string
	UserID  string

	Name    string
	Email   string
	Issuer  string
	TokenID string

	RawToken string

	Audience    []string
	Permissions []string
}

// HasPermission reports whether the identity has the given permission.
func (id *Identity) HasPermission(p string) bool {
	if id == nil {
		return false
	}
	for _, x := range id.Permissions {
		if x == p {
			return true
		}
	}
	return false
}
