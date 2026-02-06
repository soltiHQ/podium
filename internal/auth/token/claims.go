package token

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
)

// Claims is an algorithm-agnostic representation of access token claims.
type Claims struct {
	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time

	Issuer    string
	Subject   string
	TokenID   string
	UserID    string
	SessionID string

	Audience    []string
	Permissions []kind.Permission
}
