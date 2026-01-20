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
