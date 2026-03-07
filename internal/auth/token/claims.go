package token

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
)

// Claims is an algorithm-agnostic representation of access token claims.
//
// Claims is the canonical in-memory form used across token issuers/verifiers,
// independent of token format (e.g., JWT) and signing algorithm.
//
// Contract:
//   - Time fields (IssuedAt/NotBefore/ExpiresAt) must use the same clock basis.
//   - ExpiresAt must be strictly after IssuedAt/NotBefore for a valid token.
//   - Audience may be empty only if the system does not enforce audience validation.
//   - consumers must treat Permissions as a set (no semantic meaning in duplicates).
//   - Fields are intended for access tokens (short-lived) and must not contain secrets.
type Claims struct {
	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time

	Issuer    string
	Subject   string
	TokenID   string
	UserID    string
	SessionID string
	Name      string

	Audience    []string
	Permissions []kind.Permission
}
