package password

import "errors"

var (
	// ErrInvalidCredentials is returned for any subject/password mismatch (no field leaks).
	ErrInvalidCredentials = errors.New("password: invalid credentials")
)
