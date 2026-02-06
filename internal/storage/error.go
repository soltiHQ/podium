package storage

import "errors"

var (
	// ErrNotFound indicates the requested record does not exist.
	//
	// Callers should handle this explicitly, typically returning 404 at API boundaries.
	// Implementations must wrap backend-specific "not found" conditions using errors.Is.
	ErrNotFound = errors.New("storage: not found")
	// ErrAlreadyExists indicates a create operation conflicts with an existing record.
	//
	// Raised when unique constraints are violated (duplicate keys, conflicting identifiers).
	// Implementations should return this for conceptual "create" operations only.
	ErrAlreadyExists = errors.New("storage: already exists")
	// ErrConflict indicates the operation failed due to concurrent modifications.
	//
	// Common scenarios: optimistic locking failures, version mismatches, incompatible state.
	// Implementations may attach additional context via error wrapping while preserving errors.Is compatibility.
	ErrConflict = errors.New("storage: conflict")
	// ErrInvalidArgument indicates caller-provided arguments are unacceptable.
	//
	// This is distinct from domain-level validation errors which should be caught earlier
	// (e.g., in domain constructors like model.NewAgent()).
	ErrInvalidArgument = errors.New("storage: invalid argument")
	// ErrNotSupported indicates that the operation is not supported by the storage backend.
	//
	// Useful when a storage implementation is intentionally partial (e.g., inmemory in early stages)
	// or when a backend does not support certain query patterns.
	ErrNotSupported = errors.New("storage: not supported")
	// ErrUnavailable indicates the backend is temporarily unavailable.
	//
	// Examples: database is down, network partition, dependency not ready.
	// Callers may retry with backoff when this is returned.
	ErrUnavailable = errors.New("storage: unavailable")
	// ErrInternal indicates an unexpected storage-layer failure.
	//
	// Callers should treat this as unrecoverable and typically surface it as 5xx at boundaries.
	ErrInternal = errors.New("storage: internal error")
)
