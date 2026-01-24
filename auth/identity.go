package auth

import "time"

// Identity describes an authenticated principal (user/service).
type Identity struct {
	// Audience is the list of intended audiences (usually JWT "aud").
	Audience []string
	// Scopes contains granted scopes/permissions, if any.
	Scopes []string
	// Roles contains assigned roles, if any.
	Roles []string

	// ExpiresAt is the time after which the identity must not be accepted.
	ExpiresAt time.Time
	// NotBefore is the time before which the identity must not be accepted.
	NotBefore time.Time
	// IssuedAt is the token issue time.
	IssuedAt time.Time

	// Subject is a stable principal identifier (usually JWT "sub").
	Subject string
	// Name is a human-readable display name, if available.
	Name string
	// Email is the primary email address, if available.
	Email string
	// Issuer is the token issuer identifier (usually JWT "iss").
	Issuer string
	// TokenID is a unique token identifier (usually JWT "jti"), if present.
	TokenID string
	// RawToken optionally holds the raw token string used to construct this identity.
	// This is useful for logging/debugging, but should not be logged at INFO level.
	RawToken string
}

// HasScope reports whether the identity has the given scope.
func (id *Identity) HasScope(scope string) bool {
	if id == nil {
		return false
	}
	for _, s := range id.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// HasRole reports whether the identity has the given role.
func (id *Identity) HasRole(role string) bool {
	if id == nil {
		return false
	}
	for _, r := range id.Roles {
		if r == role {
			return true
		}
	}
	return false
}
