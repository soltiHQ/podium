package session

import "errors"

var (
	// ErrInvalidCredentials indicates subject/password mismatch (no field leaks).
	ErrInvalidCredentials = errors.New("session: invalid credentials")
	// ErrInvalidRequest indicates login/refresh input is malformed.
	ErrInvalidRequest = errors.New("session: invalid request")
	// ErrInvalidRefresh indicates the refresh token is invalid (mismatch / revoked / expired).
	ErrInvalidRefresh = errors.New("session: invalid refresh")
	// ErrUnauthorized indicates the session is not authorized for the requested operation.
	ErrUnauthorized = errors.New("session: unauthorized")
	// ErrRevoked indicates session was revoked.
	ErrRevoked = errors.New("session: revoked")
)
