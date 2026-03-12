package domain

import "errors"

var (
	// ErrInvalidSubject indicates that a subject is invalid or empty.
	ErrInvalidSubject = errors.New("subject cannot be empty")
	// ErrInvalidEmail indicates that the email address format is invalid.
	ErrInvalidEmail = errors.New("invalid email address")
	// ErrEmptyUserID indicates that user ID is empty.
	ErrEmptyUserID = errors.New("user id cannot be empty")
	// ErrFieldEmpty indicates that a required field is empty.
	ErrFieldEmpty = errors.New("field cannot be empty")
	// ErrEmptyName indicates that the entity name is empty.
	ErrEmptyName = errors.New("name cannot be empty")
	// ErrEmptyID indicates that entity ID is empty.
	ErrEmptyID = errors.New("id cannot be empty")
	// ErrUnknownEndpointType indicates an unrecognized endpoint type value.
	ErrUnknownEndpointType = errors.New("unknown endpoint type")
)
