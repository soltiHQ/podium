package rbac

import "errors"

var (
	// ErrInvalidArgument indicates a caller bug (nil resolver, nil user, etc.).
	ErrInvalidArgument = errors.New("rbac: invalid argument")
	// ErrUnauthorized indicates the principal has no effective permissions.
	ErrUnauthorized = errors.New("rbac: unauthorized")
)
