package storage

// AgentFilter defines a backend-specific query object for agents.
//
// A filter must be constructed by the same storage backend that consumes it.
// Passing a filter created for a different backend must return ErrInvalidArgument.
//
// Note: This is enforced by backend implementations via type assertion
// to their concrete filter type (e.g. inmemory.AgentFilter).
type AgentFilter interface{}

// UserFilter defines a backend-specific query object for users.
//
// A filter must be constructed by the same storage backend that consumes it.
// Passing a filter created for a different backend must return ErrInvalidArgument.
//
// Note: This is enforced by backend implementations via type assertion
// to their concrete filter type (e.g. inmemory.UserFilter).
type UserFilter interface{}

// RoleFilter defines a backend-specific query object for roles.
//
// A filter must be constructed by the same storage backend that consumes it.
// Passing a filter created for a different backend must return ErrInvalidArgument.
//
// Note: This is enforced by backend implementations via type assertion
// to their concrete filter type (e.g. inmemory.RoleFilter).
type RoleFilter interface{}
