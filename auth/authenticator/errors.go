package authenticator

import "errors"

var (
	// ErrInvalidCredentials is returned when subject/password are incorrect.
	ErrInvalidCredentials = errors.New("authenticator: invalid credentials")
	// ErrUnauthorized is returned when authentication succeeded but the principal has no permissions.
	ErrUnauthorized = errors.New("authenticator: unauthorized")
)
