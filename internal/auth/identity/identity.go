package identity

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
)

// Identity describes an authenticated principal (user/service) and its effective authorization data.
// It is designed to be embedded into access tokens and attached to request context.
type Identity struct {
	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time

	Issuer    string
	Subject   string
	UserID    string
	TokenID   string
	SessionID string

	Name  string
	Email string

	Audience    []string
	Permissions []kind.Permission

	// RawToken is populated by token verifiers (middleware).
	RawToken string
}

// HasPermission reports whether the identity grants the given permission.
// Wildcards are intentionally NOT supported here; if you want them, do it in RBAC explicitly.
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
