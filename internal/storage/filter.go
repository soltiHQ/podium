package storage

// AgentFilter defines a storage-agnostic abstraction for querying agents.
//
// This is a marker interface that prevents external implementations and ensures
// type safety across different storage backends. Each implementation provides
// its own concrete filter type with specific query capabilities:
//
//   - storage/inmemory.Filter: predicate-based filtering
//   - storage/postgres.Filter: SQL WHERE clause construction
//
// Callers must use the filter constructor from their target storage implementation.
// Passing a filter from one implementation to another results in ErrInvalidArgument.
type AgentFilter interface {
	// IsAgentFilter is a marker method preventing external implementations.
	IsAgentFilter()
}

// UserFilter defines a storage-agnostic abstraction for querying users.
//
// This is a marker interface that prevents external implementations and ensures
// type safety across different storage backends. Each implementation provides
// its own concrete filter type with specific query capabilities:
//
//   - storage/inmemory.UserFilter: predicate-based filtering
//   - storage/postgres.UserFilter: SQL WHERE clause construction
//
// Callers must use the filter constructor from their target storage implementation.
// Passing a filter from one implementation to another results in ErrInvalidArgument.
type UserFilter interface {
	// IsUserFilter is a marker method preventing external implementations.
	IsUserFilter()
}

// RoleFilter defines a storage-agnostic abstraction for querying roles.
//
// This is a marker interface that prevents external implementations and ensures
// type safety across different storage backends. Each implementation provides
// its own concrete filter type with specific query capabilities:
//
//   - storage/inmemory.RoleFilter: predicate-based filtering
//   - storage/postgres.RoleFilter: SQL WHERE clause construction
//
// Callers must use the filter constructor from their target storage implementation.
// Passing a filter from one implementation to another results in ErrInvalidArgument.
type RoleFilter interface {
	// IsRoleFilter is a marker method preventing external implementations.
	IsRoleFilter()
}
