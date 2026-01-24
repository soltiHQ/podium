package auth

import (
	"context"
	"errors"
)

var (
	// ErrInvalidToken is returned when the token cannot be parsed or verified.
	ErrInvalidToken = errors.New("auth: invalid token")

	// ErrExpiredToken is returned when the token is structurally valid but expired.
	ErrExpiredToken = errors.New("auth: token expired")

	// ErrUnauthorized is returned when the token is valid but not authorized
	// for the requested operation (wrong audience, missing scopes, etc.).
	ErrUnauthorized = errors.New("auth: unauthorized")
)

// Verifier validates a raw token string and returns an authenticated identity.
// Concrete implementations may use JWT, opaque tokens, mTLS certs, etc.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*Identity, error)
}
