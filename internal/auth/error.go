package auth

import "errors"

var (
	// ErrMissingPasswordHash indicates that a password credential
	// does not contain a stored password hash.
	// This usually means the credential is corrupted or improperly initialized.
	ErrMissingPasswordHash = errors.New("auth: missing password hash")
	// ErrPasswordMismatch indicates that the provided plaintext password
	// does not match the stored password hash.
	// This error must not leak which part of the credentials was invalid.
	ErrPasswordMismatch = errors.New("auth: password mismatch")
	// ErrInvalidCredentials indicates that authentication failed due to
	//  an invalid subject / password (or equivalent credential pair).
	// This error intentionally does not reveal which field was incorrect.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrWrongAuthKind indicates that an operation was attempted on a credential
	// of an incompatible authentication kind (e.g., password logic on API key).
	ErrWrongAuthKind = errors.New("auth: wrong auth kind")
	// ErrUnauthorized indicates that authentication succeeded,
	// but the principal is not authorized to perform the requested operation.
	ErrUnauthorized = errors.New("auth: unauthorized")
	// ErrInvalidToken indicates that an access token is malformed,
	// cannot be verified, or fails structural validation.
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrExpiredToken indicates that a token is structurally valid
	// but has expired or is not yet valid (exp/nbf claims).
	ErrExpiredToken = errors.New("auth: expired token")
	// ErrInvalidRefresh indicates that a refresh operation failed
	// due to an invalid session ID, malformed refresh token,
	// hash mismatch, or expired session.
	ErrInvalidRefresh = errors.New("auth: invalid refresh")
	// ErrRevoked indicates that the session associated with the request
	// has been explicitly revoked and is no longer valid.
	ErrRevoked = errors.New("auth: revoked")
	// ErrInvalidArgument indicates a programming error by the caller,
	// such as passing nil dependencies or invalid input parameters.
	ErrInvalidArgument = errors.New("auth: invalid argument")
	// ErrInvalidRequest indicates that the request payload is malformed,
	// incomplete, or violates expected input constraints.
	ErrInvalidRequest = errors.New("auth: invalid request")
)
