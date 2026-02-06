package identity

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
)

// Identity describes an authenticated principal (user/service) and its effective authorization data.
type Identity struct {
	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time

	Subject   string
	UserID    string
	Name      string
	Email     string
	Issuer    string
	TokenID   string
	SessionID string

	Audience    []string
	Permissions []kind.Permission
}

// HasPermission reports whether the identity grants the given permission.
func (id *Identity) HasPermission(p kind.Permission) bool {
	if id == nil || p == "" {
		return false
	}
	for _, x := range id.Permissions {
		if x == p {
			return true
		}
	}
	return false
}
