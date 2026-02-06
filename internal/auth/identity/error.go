package identity

import "errors"

var (
	// ErrInvalidIdentity indicates identity is nil or missing required fields.
	ErrInvalidIdentity = errors.New("identity: invalid identity")
)
