package grpcserver

import "errors"

var (
	// ErrNilServer indicates the grpc.Server is nil.
	ErrNilServer = errors.New("grpcserver: nil server")
	// ErrEmptyAddr indicates the listen address is empty.
	ErrEmptyAddr = errors.New("grpcserver: empty addr")
	// ErrAlreadyStarted indicates Start was called more than once.
	ErrAlreadyStarted = errors.New("grpcserver: already started")
)
