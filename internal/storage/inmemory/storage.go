package inmemory

import (
	"context"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Compile-time checks that Store implements the required interfaces.
var (
	_ storage.Storage         = (*Store)(nil)
	_ storage.AgentStore      = (*Store)(nil)
	_ storage.UserStore       = (*Store)(nil)
	_ storage.CredentialStore = (*Store)(nil)
	_ storage.RoleStore       = (*Store)(nil)
)

// Store provides an in-memory implementation of storage.Storage using GenericStore.
type Store struct {
	credentials *GenericStore[*domain.CredentialModel]
	agents      *GenericStore[*domain.AgentModel]
	roles       *GenericStore[*domain.RoleModel]
	users       *GenericStore[*domain.UserModel]
}

// New creates a new in-memory store with an empty state.
func New() *Store {
	return &Store{
		credentials: NewGenericStore[*domain.CredentialModel](),
		agents:      NewGenericStore[*domain.AgentModel](),
		roles:       NewGenericStore[*domain.RoleModel](),
		users:       NewGenericStore[*domain.UserModel](),
	}
}

// UpsertAgent inserts or fully replaces an agent.
//
// Delegates to GenericStore, which handles cloning and validation.
// Returns storage.ErrInvalidArgument if the agent is nil or has an empty ID.
func (s *Store) UpsertAgent(ctx context.Context, a *domain.AgentModel) error {
	if a == nil {
		return storage.ErrInvalidArgument
	}
	return s.agents.Upsert(ctx, a)
}

// GetAgent retrieves an agent by ID.
//
// Returns a deep clone to prevent external mutations affecting the stored state.
// Returns storage.ErrNotFound if no agent exists, storage.ErrInvalidArgument for empty IDs.
func (s *Store) GetAgent(ctx context.Context, id string) (*domain.AgentModel, error) {
	return s.agents.Get(ctx, id)
}

// ListAgents retrieves agents with filtering and cursor-based pagination.
//
// Filtering:
//   - Pass nil filter to retrieve all agents.
//   - Pass *inmemory.Filter created via NewFilter() for predicate-based filtering.
//   - Passing filters from other storage implementations returns storage.ErrInvalidArgument.
//
// Pagination:
//   - Results are ordered by (UpdatedAt DESC, ID ASC) for stable cursor navigation.
//   - Cursor is an opaque base64-encoded token containing position information.
//   - Invalid or corrupted cursors return storage.ErrInvalidArgument.
//
// All returned agents are deep clones isolated from the internal state.
func (s *Store) ListAgents(ctx context.Context, filter storage.AgentFilter, opts storage.ListOptions) (*storage.AgentListResult, error) {
	var predicate func(*domain.AgentModel) bool

	if filter != nil {
		f, ok := filter.(*Filter)
		if !ok {
			return nil, storage.ErrInvalidArgument
		}
		predicate = f.Matches
	}

	result, err := s.agents.List(ctx, predicate, opts)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteAgent removes an agent by ID.
//
// Returns storage.ErrNotFound if the agent doesn't exist, storage.ErrInvalidArgument for empty IDs.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	return s.agents.Delete(ctx, id)
}

// UpsertUser inserts or fully replaces a user.
//
// Delegates to GenericStore, which handles cloning and validation.
// Returns storage.ErrInvalidArgument if the user is nil or has an empty ID.
func (s *Store) UpsertUser(ctx context.Context, u *domain.UserModel) error {
	if u == nil {
		return storage.ErrInvalidArgument
	}
	return s.users.Upsert(ctx, u)
}

// GetUser retrieves a user by ID.
//
// Returns a deep clone to prevent external mutations affecting the stored state.
// Returns storage.ErrNotFound if no user exists, storage.ErrInvalidArgument for empty IDs.
func (s *Store) GetUser(ctx context.Context, id string) (*domain.UserModel, error) {
	return s.users.Get(ctx, id)
}

// GetUserBySubject retrieves a user by their subject identifier.
//
// This method performs a linear scan and is O(n) - acceptable for in-memory implementation
// with small datasets. Production implementations should use indexed lookups.
//
// Returns storage.ErrNotFound if no user with the subject exists, storage.ErrInvalidArgument for empty subject.
func (s *Store) GetUserBySubject(ctx context.Context, subject string) (*domain.UserModel, error) {
	if subject == "" {
		return nil, storage.ErrInvalidArgument
	}

	result, err := s.users.List(ctx, func(u *domain.UserModel) bool {
		return u.Subject() == subject
	}, storage.ListOptions{Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, storage.ErrNotFound
	}
	return result.Items[0], nil
}

// ListUsers retrieves users with filtering and cursor-based pagination.
//
// Filtering:
//   - Pass nil filter to retrieve all users.
//   - Pass *inmemory.UserFilter created via NewUserFilter() for predicate-based filtering.
//   - Passing filters from other storage implementations returns storage.ErrInvalidArgument.
//
// Pagination:
//   - Results are ordered by (UpdatedAt DESC, ID ASC) for stable cursor navigation.
//   - Cursor is an opaque base64-encoded token containing position information.
//   - Invalid or corrupted cursors return storage.ErrInvalidArgument.
//
// All returned users are deep clones isolated from the internal state.
func (s *Store) ListUsers(ctx context.Context, filter storage.UserFilter, opts storage.ListOptions) (*storage.UserListResult, error) {
	var predicate func(*domain.UserModel) bool

	if filter != nil {
		f, ok := filter.(*UserFilter)
		if !ok {
			return nil, storage.ErrInvalidArgument
		}
		predicate = f.Matches
	}
	result, err := s.users.List(ctx, predicate, opts)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteUser removes a user by ID.
//
// Returns storage.ErrNotFound if the user doesn't exist, storage.ErrInvalidArgument for empty IDs.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.users.Delete(ctx, id)
}

// UpsertCredential inserts or fully replaces a credential.
//
// Delegates to GenericStore, which handles cloning and validation.
// Returns storage.ErrInvalidArgument if the credential is nil or has an empty ID.
func (s *Store) UpsertCredential(ctx context.Context, c *domain.CredentialModel) error {
	if c == nil {
		return storage.ErrInvalidArgument
	}
	return s.credentials.Upsert(ctx, c)
}

// GetCredential retrieves a credential by ID.
//
// Returns a deep clone to prevent external mutations affecting the stored state.
// Returns storage.ErrNotFound if no credential exists, storage.ErrInvalidArgument for empty IDs.
func (s *Store) GetCredential(ctx context.Context, id string) (*domain.CredentialModel, error) {
	return s.credentials.Get(ctx, id)
}

// GetCredentialByUserAndType retrieves a specific credential type for a user.
// Returns storage.ErrNotFound if no matching credential exists, storage.ErrInvalidArgument for empty userID.
func (s *Store) GetCredentialByUserAndType(ctx context.Context, userID string, credType domain.CredentialType) (*domain.CredentialModel, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}

	result, err := s.credentials.List(ctx, func(c *domain.CredentialModel) bool {
		return c.UserID() == userID && c.Type() == credType
	}, storage.ListOptions{Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, storage.ErrNotFound
	}
	return result.Items[0], nil
}

// ListCredentialsByUser retrieves all credentials for a specific user.
//
// This method performs a linear scan and is O(n) - acceptable for in-memory implementation
// with small datasets. Production implementations should use indexed lookups.
//
// Returns storage.ErrInvalidArgument for empty userID.
func (s *Store) ListCredentialsByUser(ctx context.Context, userID string) ([]*domain.CredentialModel, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}

	result, err := s.credentials.List(ctx, func(c *domain.CredentialModel) bool {
		return c.UserID() == userID
	}, storage.ListOptions{Limit: storage.MaxListLimit})
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

// DeleteCredential removes a credential by ID.
//
// Returns storage.ErrNotFound if the credential doesn't exist, storage.ErrInvalidArgument for empty IDs.
func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	return s.credentials.Delete(ctx, id)
}

// UpsertRole inserts or fully replaces a role.
//
// Delegates to GenericStore, which handles cloning and validation.
// Returns storage.ErrInvalidArgument if the role is nil or has an empty ID.
func (s *Store) UpsertRole(ctx context.Context, r *domain.RoleModel) error {
	if r == nil {
		return storage.ErrInvalidArgument
	}
	return s.roles.Upsert(ctx, r)
}

// GetRole retrieves a role by ID.
//
// Returns a deep clone to prevent external mutations affecting the stored state.
// Returns storage.ErrNotFound if no role exists, storage.ErrInvalidArgument for empty IDs.
func (s *Store) GetRole(ctx context.Context, id string) (*domain.RoleModel, error) {
	return s.roles.Get(ctx, id)
}

// GetRoles retrieves roles by their IDs.
//
// Returns storage.ErrInvalidArgument if ids are empty or contain empty elements.
// Returns storage.ErrNotFound if any role is missing.
func (s *Store) GetRoles(ctx context.Context, ids []string) ([]*domain.RoleModel, error) {
	if len(ids) == 0 {
		return nil, storage.ErrInvalidArgument
	}
	for _, id := range ids {
		if id == "" {
			return nil, storage.ErrInvalidArgument
		}
	}

	// Deduplicate but preserve order of first occurrence.
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	out := make([]*domain.RoleModel, 0, len(unique))
	for _, id := range unique {
		r, err := s.roles.Get(ctx, id)
		if err != nil {
			return nil, storage.ErrNotFound
		}
		out = append(out, r)
	}
	return out, nil
}

// GetRoleByName retrieves a role by its name.
//
// This method performs a linear scan and is O(n) - acceptable for in-memory implementation
// with small datasets. Production implementations should use indexed lookups.
//
// Returns storage.ErrNotFound if no role with the name exists, storage.ErrInvalidArgument for empty name.
func (s *Store) GetRoleByName(ctx context.Context, name string) (*domain.RoleModel, error) {
	if name == "" {
		return nil, storage.ErrInvalidArgument
	}

	result, err := s.roles.List(ctx, func(r *domain.RoleModel) bool {
		return r.Name() == name
	}, storage.ListOptions{Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, storage.ErrNotFound
	}
	return result.Items[0], nil
}

// ListRoles retrieves roles with filtering and cursor-based pagination.
//
// Filtering:
//   - Pass nil filter to retrieve all roles.
//   - Pass *inmemory.RoleFilter created via NewRoleFilter() for predicate-based filtering.
//   - Passing filters from other storage implementations returns storage.ErrInvalidArgument.
//
// Pagination:
//   - Results are ordered by (UpdatedAt DESC, ID ASC) for stable cursor navigation.
//   - Cursor is an opaque base64-encoded token containing position information.
//   - Invalid or corrupted cursors return storage.ErrInvalidArgument.
//
// All returned roles are deep clones isolated from the internal state.
func (s *Store) ListRoles(ctx context.Context, filter storage.RoleFilter, opts storage.ListOptions) (*storage.RoleListResult, error) {
	var predicate func(*domain.RoleModel) bool

	if filter != nil {
		f, ok := filter.(*RoleFilter)
		if !ok {
			return nil, storage.ErrInvalidArgument
		}
		predicate = f.Matches
	}

	result, err := s.roles.List(ctx, predicate, opts)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteRole removes a role by ID.
//
// Returns storage.ErrNotFound if the role doesn't exist, storage.ErrInvalidArgument for empty IDs.
func (s *Store) DeleteRole(ctx context.Context, id string) error {
	return s.roles.Delete(ctx, id)
}
