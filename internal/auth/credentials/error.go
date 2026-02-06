package credentials

import "errors"

var (
	// ErrWrongAuthKind indicates password ops were used for a non-password credential.
	ErrWrongAuthKind = errors.New("credentials: wrong auth kind")
	// ErrPasswordMismatch indicates the provided password does not match the stored hash.
	ErrPasswordMismatch = errors.New("credentials: password mismatch")
	// ErrMissingPasswordHash indicates password credential lacks a stored hash.
	ErrMissingPasswordHash = errors.New("credentials: missing password hash")
)
