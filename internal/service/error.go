package service

import "errors"

var (
	// ErrNilService indicates a required service dependency is nil.
	ErrNilService = errors.New("service: nil service")
)
