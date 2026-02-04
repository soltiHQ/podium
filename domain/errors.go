package domain

import "errors"

var (
	// ErrNilSyncRequest indicates that sync request is nil.
	ErrNilSyncRequest = errors.New("sync request cannot be nil")
	// ErrFieldEmpty indicates that a required field is empty.
	ErrFieldEmpty = errors.New("field cannot be empty")
	// ErrInvalidURL indicates that the url format is invalid.
	ErrInvalidURL = errors.New("invalid URL format")
	// ErrEmptyID indicates that entity ID is empty.
	ErrEmptyID = errors.New("id cannot be empty")
	// ErrEmptyName indicates that the entity name is empty.
	ErrEmptyName = errors.New("name cannot be empty")
	// ErrInvalidSubject indicates that a subject is invalid or empty.
	ErrInvalidSubject = errors.New("subject cannot be empty")
)
