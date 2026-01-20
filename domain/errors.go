package domain

import "errors"

var (
	// ErrNilSyncRequest indicates that sync request is nil.
	ErrNilSyncRequest = errors.New("sync request cannot be nil")
	// ErrFieldEmpty indicates that the field is empty.
	ErrFieldEmpty = errors.New("field cannot be empty")
	// ErrInvalidURL indicates that the url format is invalid.
	ErrInvalidURL = errors.New("endpoint must be 'scheme://host' or 'host:port'")
)
