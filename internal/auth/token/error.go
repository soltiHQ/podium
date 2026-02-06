package token

import "errors"

var (
	// ErrInvalidToken is returned when the token cannot be parsed or verified.
	ErrInvalidToken = errors.New("token: invalid token")
	// ErrExpiredToken is returned when the token is structurally valid but expired / not valid yet.
	ErrExpiredToken = errors.New("token: expired token")
	// ErrUnauthorized is returned when the token is valid but not authorized
	// for the verifier policy (issuer/audience/etc).
	ErrUnauthorized = errors.New("token: unauthorized")
)
