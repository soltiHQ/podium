// Package inmemory provides an in-process, in-memory implementation of the storage layer.
//
// This backend is intended for:
//   - unit/integration tests where you want a real Store without external dependencies;
//   - local development / demos;
//   - ephemeral control-plane instances where durability is not required.
//
// Guarantees
//
//   - Thread-safety: all operations are safe for concurrent use.
//
//   - Isolation: objects returned from Get/List/GetMany are deep-cloned, so caller mutations
//     never affect the stored state.
//
//   - Deterministic listing: all list operations are ordered by
//
//     (UpdatedAt DESC, ID ASC)
//
//     and support cursor-based pagination.
//
//   - Cursor opacity: cursors are backend-defined opaque tokens. Callers must not parse
//     or construct cursors manually; treat them as black boxes.
//
// # Filters
//
//	are backend-specific query objects.
//
// The memory backend provides builder-style filter types (AgentFilter/UserFilter/RoleFilter)
// which implement the corresponding storage.*Filter interfaces.
//
// Contract:
//
//   - A filter must be constructed by the same backend that consumes it.
//   - If a caller passes a filter created for a different backend (or any other type),
//     the backend must return storage.ErrInvalidArgument.
//   - In inmemory this is enforced by type assertions (e.g. filter.(*AgentFilter)).
//
// # Error semantics
//
// The inmemory backend is not expected to produce storage.ErrUnavailable, as it does not
// communicate with external systems. Methods may still return ErrInvalidArgument, ErrNotFound,
// ErrAlreadyExists, or ErrInternal.
//
// # Uniqueness invariants
//
// Some lookups require uniqueness that a real database would normally enforce via UNIQUE indexes.
// If these invariants are violated (e.g. non-unique subject, userID+auth, credentialID, role name),
// the backend returns storage.ErrInternal to signal corrupted state / broken invariant.
//
// # Pagination cursor format
//
// The cursor encodes (UpdatedAtUnixNano, ID) plus backend/version metadata, and is base64-url encoded.
// The exact format is considered an implementation detail and may change in future versions.
package inmemory
