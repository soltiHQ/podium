package service

import "errors"

// ErrNilService indicates a required service dependency is nil.
var ErrNilService = errors.New("service: nil service")
