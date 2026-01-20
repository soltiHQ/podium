package storage

import (
	"context"

	"github.com/soltiHQ/control-plane/domain"
)

// AgentListResult contains a page of agent results with pagination support.
type AgentListResult = ListResult[*domain.AgentModel]

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
	//   - Cursor-based pagination works correctly across requests.
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

// Storage aggregates all domain-specific storage capabilities.
type Storage interface {
	AgentStore
}
