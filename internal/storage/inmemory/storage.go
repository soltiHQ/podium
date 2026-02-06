package inmemory

import (
	"context"
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Compile-time checks that Store implements the required interfaces.
var (
	_ storage.Storage         = (*Store)(nil)
	_ storage.AgentStore      = (*Store)(nil)
	_ storage.UserStore       = (*Store)(nil)
	_ storage.CredentialStore = (*Store)(nil)
	_ storage.RoleStore       = (*Store)(nil)
	_ storage.VerifierStore   = (*Store)(nil)
	_ storage.SessionStore    = (*Store)(nil)
)

// Store provides an in-memory implementation of storage.Storage using GenericStore.
type Store struct {
	agents      *GenericStore[*model.Agent]
	users       *GenericStore[*model.User]
	roles       *GenericStore[*model.Role]
	credentials *GenericStore[*model.Credential]
	verifiers   *GenericStore[*model.Verifier]
	sessions    *GenericStore[*model.Session]
}

// New creates a new in-memory store with an empty state.
func New() *Store {
	return &Store{
		agents:      NewGenericStore[*model.Agent](),
		users:       NewGenericStore[*model.User](),
		roles:       NewGenericStore[*model.Role](),
		credentials: NewGenericStore[*model.Credential](),
		verifiers:   NewGenericStore[*model.Verifier](),
		sessions:    NewGenericStore[*model.Session](),
	}
}

// --- Agents ---

func (s *Store) UpsertAgent(ctx context.Context, a *model.Agent) error {
	if a == nil {
		return storage.ErrInvalidArgument
	}
	return s.agents.Upsert(ctx, a)
}

func (s *Store) GetAgent(ctx context.Context, id string) (*model.Agent, error) {
	return s.agents.Get(ctx, id)
}

func (s *Store) ListAgents(ctx context.Context, filter storage.AgentFilter, opts storage.ListOptions) (*storage.AgentListResult, error) {
	var predicate func(*model.Agent) bool

	if filter != nil {
		f, ok := filter.(*AgentFilter)
		if !ok {
			return nil, storage.ErrInvalidArgument
		}
		predicate = f.Matches
	}
	return s.agents.List(ctx, predicate, opts)
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	return s.agents.Delete(ctx, id)
}

// --- Users ---

func (s *Store) UpsertUser(ctx context.Context, u *model.User) error {
	if u == nil {
		return storage.ErrInvalidArgument
	}
	return s.users.Upsert(ctx, u)
}

func (s *Store) GetUser(ctx context.Context, id string) (*model.User, error) {
	return s.users.Get(ctx, id)
}

func (s *Store) GetUserBySubject(ctx context.Context, subject string) (*model.User, error) {
	if subject == "" {
		return nil, storage.ErrInvalidArgument
	}

	res, err := s.users.List(ctx, func(u *model.User) bool {
		return u.Subject() == subject
	}, storage.ListOptions{Limit: 2})
	if err != nil {
		return nil, err
	}

	switch len(res.Items) {
	case 0:
		return nil, storage.ErrNotFound
	case 1:
		return res.Items[0], nil
	default:
		return nil, storage.ErrConflict
	}
}

func (s *Store) ListUsers(ctx context.Context, filter storage.UserFilter, opts storage.ListOptions) (*storage.UserListResult, error) {
	var predicate func(*model.User) bool

	if filter != nil {
		f, ok := filter.(*UserFilter)
		if !ok {
			return nil, storage.ErrInvalidArgument
		}
		predicate = f.Matches
	}

	return s.users.List(ctx, predicate, opts)
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.users.Delete(ctx, id)
}

// --- Credentials ---

func (s *Store) UpsertCredential(ctx context.Context, c *model.Credential) error {
	if c == nil {
		return storage.ErrInvalidArgument
	}
	return s.credentials.Upsert(ctx, c)
}

func (s *Store) GetCredential(ctx context.Context, id string) (*model.Credential, error) {
	return s.credentials.Get(ctx, id)
}

func (s *Store) GetCredentialByUserAndAuth(ctx context.Context, userID string, auth kind.Auth) (*model.Credential, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}

	res, err := s.credentials.List(ctx, func(c *model.Credential) bool {
		return c.UserID() == userID && c.AuthKind() == auth
	}, storage.ListOptions{Limit: 2})
	if err != nil {
		return nil, err
	}

	switch len(res.Items) {
	case 0:
		return nil, storage.ErrNotFound
	case 1:
		return res.Items[0], nil
	default:
		return nil, storage.ErrConflict
	}
}

func (s *Store) ListCredentialsByUser(ctx context.Context, userID string) ([]*model.Credential, error) {
	if userID == "" {
		return nil, storage.ErrInvalidArgument
	}

	res, err := s.credentials.List(ctx, func(c *model.Credential) bool {
		return c.UserID() == userID
	}, storage.ListOptions{Limit: storage.MaxListLimit})
	if err != nil {
		return nil, err
	}

	return res.Items, nil
}

func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	return s.credentials.Delete(ctx, id)
}

// --- Verifiers ---

func (s *Store) UpsertVerifier(ctx context.Context, v *model.Verifier) error {
	if v == nil {
		return storage.ErrInvalidArgument
	}
	return s.verifiers.Upsert(ctx, v)
}

func (s *Store) GetVerifier(ctx context.Context, id string) (*model.Verifier, error) {
	return s.verifiers.Get(ctx, id)
}

func (s *Store) GetVerifierByCredential(ctx context.Context, credentialID string) (*model.Verifier, error) {
	if credentialID == "" {
		return nil, storage.ErrInvalidArgument
	}

	res, err := s.verifiers.List(ctx, func(v *model.Verifier) bool {
		return v.CredentialID() == credentialID
	}, storage.ListOptions{Limit: 2})
	if err != nil {
		return nil, err
	}

	switch len(res.Items) {
	case 0:
		return nil, storage.ErrNotFound
	case 1:
		return res.Items[0], nil
	default:
		return nil, storage.ErrConflict
	}
}

func (s *Store) DeleteVerifier(ctx context.Context, id string) error {
	return s.verifiers.Delete(ctx, id)
}

// --- Sessions ---

func (s *Store) CreateSession(ctx context.Context, sess *model.Session) error {
	if sess == nil {
		return storage.ErrInvalidArgument
	}
	return s.sessions.Create(ctx, sess)
}

func (s *Store) GetSession(ctx context.Context, id string) (*model.Session, error) {
	return s.sessions.Get(ctx, id)
}

func (s *Store) RotateRefresh(ctx context.Context, sessionID string, newHash []byte, newExpiresAt time.Time) error {
	if sessionID == "" || len(newHash) == 0 || newExpiresAt.IsZero() {
		return storage.ErrInvalidArgument
	}
	return s.sessions.Update(ctx, sessionID, func(cur *model.Session) (*model.Session, error) {
		if err := cur.SetRefreshHash(newHash); err != nil {
			return nil, storage.ErrInvalidArgument
		}
		if err := cur.SetExpiresAt(newExpiresAt); err != nil {
			return nil, storage.ErrInvalidArgument
		}
		return cur, nil
	})
}

func (s *Store) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time) error {
	if sessionID == "" || revokedAt.IsZero() {
		return storage.ErrInvalidArgument
	}
	return s.sessions.Update(ctx, sessionID, func(cur *model.Session) (*model.Session, error) {
		if err := cur.Revoke(revokedAt); err != nil {
			return nil, storage.ErrInvalidArgument
		}
		return cur, nil
	})
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return s.sessions.Delete(ctx, id)
}

// --- Roles ---

func (s *Store) UpsertRole(ctx context.Context, r *model.Role) error {
	if r == nil {
		return storage.ErrInvalidArgument
	}
	return s.roles.Upsert(ctx, r)
}

func (s *Store) GetRole(ctx context.Context, id string) (*model.Role, error) {
	return s.roles.Get(ctx, id)
}

func (s *Store) GetRoles(ctx context.Context, ids []string) ([]*model.Role, error) {
	if len(ids) == 0 {
		return nil, storage.ErrInvalidArgument
	}
	for _, id := range ids {
		if id == "" {
			return nil, storage.ErrInvalidArgument
		}
	}

	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	out := make([]*model.Role, 0, len(unique))
	for _, id := range unique {
		r, err := s.roles.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) GetRoleByName(ctx context.Context, name string) (*model.Role, error) {
	if name == "" {
		return nil, storage.ErrInvalidArgument
	}

	res, err := s.roles.List(ctx, func(r *model.Role) bool {
		return r.Name() == name
	}, storage.ListOptions{Limit: 2})
	if err != nil {
		return nil, err
	}

	switch len(res.Items) {
	case 0:
		return nil, storage.ErrNotFound
	case 1:
		return res.Items[0], nil
	default:
		return nil, storage.ErrConflict
	}
}

func (s *Store) ListRoles(ctx context.Context, filter storage.RoleFilter, opts storage.ListOptions) (*storage.RoleListResult, error) {
	var predicate func(*model.Role) bool

	if filter != nil {
		f, ok := filter.(*RoleFilter)
		if !ok {
			return nil, storage.ErrInvalidArgument
		}
		predicate = f.Matches
	}
	return s.roles.List(ctx, predicate, opts)
}

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	return s.roles.Delete(ctx, id)
}
