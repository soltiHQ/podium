// Package storage defines persistence interfaces for control-plane domain objects.
package storage

import (
	"context"

	"github.com/soltiHQ/control-plane/domain"
)

// AgentListResult contains a page of agent results with pagination support.
type AgentListResult = ListResult[*domain.AgentModel]

// UserListResult contains a page of user results with pagination support.
type UserListResult = ListResult[*domain.UserModel]

// CredentialListResult contains a page of credential results with pagination support.
type CredentialListResult = ListResult[*domain.CredentialModel]

// RoleListResult contains a page of role results with pagination support.
type RoleListResult = ListResult[*domain.RoleModel]

// AgentStore defines persistence operations for agent domain objects.
type AgentStore interface {
	// UpsertAgent creates a new agent or replaces an existing one.
	//
	// If an agent with the same ID exists, it is fully replaced.
	// Otherwise, a new agent record is created.
	//
	// Returns:
	//   - ErrInvalidArgument if the agent violates storage-level invariants.
	//   - ErrInternal for unexpected storage failures.
	UpsertAgent(ctx context.Context, a *domain.AgentModel) error

	// GetAgent retrieves an agent by its unique identifier.
	//
	// Returns:
	//   - ErrNotFound if no agent with the given ID exists.
	//   - ErrInvalidArgument if the ID format is invalid.
	//   - ErrInternal for unexpected storage failures.
	GetAgent(ctx context.Context, id string) (*domain.AgentModel, error)

	// ListAgents retrieves agents matching the provided filter with pagination support.
	//
	// Results are ordered by (UpdatedAt DESC, ID ASC) to ensure:
	//   - Recently updated agents appear first.
	//   - Stable ordering when UpdatedAt values are identical.
	//   - Cursor-based pagination works correctly across request.
	//
	// The filter parameter is implementation-specific. Pass nil to retrieve all agents.
	// Use filter constructors from the concrete storage package (e.g., inmemory.NewFilter()).
	//
	// Pagination is cursor-based to handle large result sets safely.
	// Clients should:
	//   1. Make an initial request with an empty Cursor.
	//   2. Check AgentListResult.NextCursor.
	//   3. If non-empty, pass it as Cursor in the next request.
	//   4. Repeat until the NextCursor is empty.
	//
	// Returns:
	//   - ErrInvalidArgument if a filter type is incompatible or the cursor is malformed.
	//   - ErrInternal for unexpected storage failures.
	ListAgents(ctx context.Context, filter AgentFilter, opts ListOptions) (*AgentListResult, error)

	// DeleteAgent removes an agent by its unique identifier.
	//
	// Returns:
	//   - ErrNotFound if no agent with the given ID exists.
	//   - ErrInvalidArgument if the ID format is invalid.
	//   - ErrInternal for unexpected storage failures.
	DeleteAgent(ctx context.Context, id string) error
}

// UserStore defines persistence operations for user domain objects.
type UserStore interface {
	// UpsertUser creates a new user or replaces an existing one.
	//
	// If a user with the same ID exists, it is fully replaced.
	// Otherwise, a new user record is created.
	//
	// Returns:
	//   - ErrInvalidArgument if the user is nil or violates storage-level invariants.
	//   - ErrInternal for unexpected storage failures.
	UpsertUser(ctx context.Context, u *domain.UserModel) error

	// GetUser retrieves a user by its unique identifier.
	//
	// Returns:
	//   - ErrNotFound if no user with the given ID exists.
	//   - ErrInvalidArgument if the ID format is invalid.
	//   - ErrInternal for unexpected storage failures.
	GetUser(ctx context.Context, id string) (*domain.UserModel, error)

	// GetUserBySubject retrieves a user by their subject identifier (e.g., OIDC sub claim).
	//
	// Subject is typically populated from the identity provider's subject claim
	// and serves as a stable, unique identifier for authentication purposes.
	//
	// Returns:
	//   - ErrNotFound if no user with the given subject exists.
	//   - ErrInvalidArgument if the subject is empty.
	//   - ErrInternal for unexpected storage failures.
	GetUserBySubject(ctx context.Context, subject string) (*domain.UserModel, error)

	// ListUsers retrieves users matching the provided filter with pagination support.
	//
	// Results are ordered by (UpdatedAt DESC, ID ASC) to ensure:
	//   - Recently updated users appear first.
	//   - Stable ordering when UpdatedAt values are identical.
	//   - Cursor-based pagination works correctly across requests.
	//
	// The filter parameter is implementation-specific. Pass nil to retrieve all users.
	// Use filter constructors from the concrete storage package (e.g., inmemory.NewUserFilter()).
	//
	// Pagination is cursor-based to handle large result sets safely.
	// Clients should:
	//   1. Make an initial request with an empty Cursor.
	//   2. Check UserListResult.NextCursor.
	//   3. If non-empty, pass it as Cursor in the next request.
	//   4. Repeat until the NextCursor is empty.
	//
	// Returns:
	//   - ErrInvalidArgument if a filter type is incompatible or the cursor is malformed.
	//   - ErrInternal for unexpected storage failures.
	ListUsers(ctx context.Context, filter UserFilter, opts ListOptions) (*UserListResult, error)

	// DeleteUser removes a user by its unique identifier.
	//
	// Returns:
	//   - ErrNotFound if no user with the given ID exists.
	//   - ErrInvalidArgument if the ID format is invalid.
	//   - ErrInternal for unexpected storage failures.
	DeleteUser(ctx context.Context, id string) error
}

// CredentialStore defines persistence operations for credential domain objects.
type CredentialStore interface {
	// UpsertCredential creates a new credential or replaces an existing one.
	//
	// If a credential with the same ID exists, it is fully replaced.
	// Otherwise, a new credential record is created.
	//
	// Returns:
	//   - ErrInvalidArgument if the credential is nil or violates storage-level invariants.
	//   - ErrInternal for unexpected storage failures.
	UpsertCredential(ctx context.Context, c *domain.CredentialModel) error

	// GetCredential retrieves a credential by its unique identifier.
	//
	// Returns:
	//   - ErrNotFound if no credential with the given ID exists.
	//   - ErrInvalidArgument if the ID format is invalid.
	//   - ErrInternal for unexpected storage failures.
	GetCredential(ctx context.Context, id string) (*domain.CredentialModel, error)

	// GetCredentialByUserAndType retrieves a specific credential type for a user.
	//
	// Returns:
	//   - ErrNotFound if no matching credential exists.
	//   - ErrInvalidArgument if the userID is empty.
	//   - ErrInternal for unexpected storage failures.
	GetCredentialByUserAndType(ctx context.Context, userID string, credType domain.CredentialType) (*domain.CredentialModel, error)

	// ListCredentialsByUser retrieves all credentials for a specific user.
	// Returns all credential types (password, OIDC, API keys) associated with the user.
	//
	// Returns:
	//   - ErrInvalidArgument if the userID is empty.
	//   - ErrInternal for unexpected storage failures.
	ListCredentialsByUser(ctx context.Context, userID string) ([]*domain.CredentialModel, error)

	// DeleteCredential removes a credential by its unique identifier.
	//
	// Returns:
	//   - ErrNotFound if no credential with the given ID exists.
	//   - ErrInvalidArgument if the ID format is invalid.
	//   - ErrInternal for unexpected storage failures.
	DeleteCredential(ctx context.Context, id string) error
}

// RoleStore defines persistence operations for role domain objects.
type RoleStore interface {
	// UpsertRole creates a new role or replaces an existing one.
	//
	// If a role with the same ID exists, it is fully replaced.
	// Otherwise, a new role record is created.
	//
	// Returns:
	//   - ErrInvalidArgument if the role is nil or violates storage-level invariants.
	//   - ErrInternal for unexpected storage failures.
	UpsertRole(ctx context.Context, r *domain.RoleModel) error

	// GetRole retrieves a role by its unique identifier.
	//
	// Returns:
	//   - ErrNotFound if no role with the given ID exists.
	//   - ErrInvalidArgument if the ID format is invalid.
	//   - ErrInternal for unexpected storage failures.
	GetRole(ctx context.Context, id string) (*domain.RoleModel, error)

	// GetRoles retrieves roles by their IDs.
	//
	// Returns:
	//   - ErrInvalidArgument if ids are empty or contain empty elements.
	//   - ErrInternal for unexpected storage failures.
	GetRoles(ctx context.Context, ids []string) ([]*domain.RoleModel, error)

	// GetRoleByName retrieves a role by its name.
	//
	// Name is a human-readable identifier and should be unique within the system.
	//
	// Returns:
	//   - ErrNotFound if no role with the given name exists.
	//   - ErrInvalidArgument if the name is empty.
	//   - ErrInternal for unexpected storage failures.
	GetRoleByName(ctx context.Context, name string) (*domain.RoleModel, error)

	// ListRoles retrieves roles matching the provided filter with pagination support.
	//
	// Results are ordered by (UpdatedAt DESC, ID ASC) to ensure:
	//   - Recently updated roles appear first.
	//   - Stable ordering when UpdatedAt values are identical.
	//   - Cursor-based pagination works correctly across requests.
	//
	// The filter parameter is implementation-specific. Pass nil to retrieve all roles.
	// Use filter constructors from the concrete storage package (e.g., inmemory.NewRoleFilter()).
	//
	// Pagination is cursor-based to handle large result sets safely.
	//
	// Returns:
	//   - ErrInvalidArgument if a filter type is incompatible or the cursor is malformed.
	//   - ErrInternal for unexpected storage failures.
	ListRoles(ctx context.Context, filter RoleFilter, opts ListOptions) (*RoleListResult, error)

	// DeleteRole removes a role by its unique identifier.
	//
	// Returns:
	//   - ErrNotFound if no role with the given ID exists.
	//   - ErrInvalidArgument if the ID format is invalid.
	//   - ErrInternal for unexpected storage failures.
	DeleteRole(ctx context.Context, id string) error
}

// Storage aggregates all domain-specific storage capabilities.
type Storage interface {
	CredentialStore
	AgentStore
	UserStore
	RoleStore
}
