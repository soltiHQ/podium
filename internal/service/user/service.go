package user

import (
	"context"
	"errors"
	"strings"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Service implements user-related use-cases on top of storage contracts.
type Service struct {
	logger zerolog.Logger
	store  storage.Storage
}

// New creates a new users service.
func New(store storage.Storage, logger zerolog.Logger) *Service {
	if store == nil {
		panic("user.Service: store is nil")
	}
	return &Service{
		logger: logger.With().Str("service", "users").Logger(),
		store:  store,
	}
}

// List returns a page of users matching the query.
func (s *Service) List(ctx context.Context, q ListQuery) (*Page, error) {
	res, err := s.store.ListUsers(ctx, q.Filter, storage.ListOptions{
		Limit:  service.NormalizeListLimit(q.Limit, defaultListLimit),
		Cursor: q.Cursor,
	})
	if err != nil {
		return nil, err
	}
	return &Page{Items: res.Items, NextCursor: res.NextCursor}, nil
}

// Get returns a single user by ID.
func (s *Service) Get(ctx context.Context, id string) (*model.User, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}
	return s.store.GetUser(ctx, id)
}

// GetBySubject returns a single user by authentication subject.
func (s *Service) GetBySubject(ctx context.Context, subject string) (*model.User, error) {
	if subject == "" {
		return nil, storage.ErrInvalidArgument
	}
	return s.store.GetUserBySubject(ctx, subject)
}

// EffectivePermissions returns the union of u's role and direct permissions.
func (s *Service) EffectivePermissions(ctx context.Context, u *model.User) (map[enum.Permission]struct{}, error) {
	return EffectivePermissions(ctx, s.store, u)
}

// Delete removes a user on behalf of actor, cascading sessions → verifiers →
// credentials → user. Another user may be deleted only when actor holds all
// of that user's permissions (ErrPermissionCeiling), and the last enabled user
// with every permission cannot be deleted (ErrLastAdmin).
func (s *Service) Delete(ctx context.Context, actor *identity.Identity, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}

	var credCount int
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		target, err := tx.GetUser(ctx, id)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			target = nil // the cascade below still clears what the user left behind
		case err != nil:
			return err
		}
		if err = CheckTarget(ctx, tx, actor, target); err != nil {
			return err
		}
		if err = keepAdmin(ctx, tx, target, nil); err != nil {
			return err
		}
		if err = tx.DeleteSessionsByUser(ctx, id); err != nil {
			return err
		}
		creds, err := tx.ListCredentialsByUser(ctx, id)
		if err != nil {
			return err
		}
		credCount = len(creds)
		for _, c := range creds {
			if c == nil {
				continue
			}
			if err = tx.DeleteVerifierByCredential(ctx, c.ID()); err != nil {
				return err
			}
			if err = tx.DeleteCredential(ctx, c.ID()); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		}
		if err = tx.DeleteUser(ctx, id); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("user_id", id).Msg("delete failed")
		return err
	}
	s.logger.Debug().Str("user_id", id).Int("credentials", credCount).Msg("user deleted")
	return nil
}

// SetDisabled disables or enables a user on behalf of actor and returns the
// stored user. Another user may be changed only when actor holds all of that
// user's permissions (ErrPermissionCeiling), and the last enabled user with
// every permission cannot be disabled (ErrLastAdmin).
func (s *Service) SetDisabled(ctx context.Context, actor *identity.Identity, id string, disabled bool) (*model.User, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}

	var out *model.User
	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		current, err := tx.GetUser(ctx, id)
		if err != nil {
			return err
		}
		if err = CheckTarget(ctx, tx, actor, current); err != nil {
			return err
		}
		next := current.Clone()
		if disabled {
			next.Disable()
		} else {
			next.Enable()
		}
		if err = keepAdmin(ctx, tx, current, next); err != nil {
			return err
		}
		out = next
		return tx.UpsertUser(ctx, next)
	})
	if err != nil {
		return nil, err
	}
	s.logger.Debug().Str("user_id", id).Bool("disabled", disabled).Msg("user status changed")
	return out, nil
}

// Create stores a new user on behalf of actor. Every permission the user
// gets, directly or through its roles, must be held by actor
// (ErrPermissionCeiling). An existing ID is storage.ErrAlreadyExists.
func (s *Service) Create(ctx context.Context, actor *identity.Identity, u *model.User) error {
	return s.save(ctx, u, func(tx storage.Storage, existing *model.User) error {
		if existing != nil {
			return storage.ErrAlreadyExists
		}
		return checkGrant(ctx, tx, actor, u)
	})
}

// Update replaces an existing user on behalf of actor. Another user may be
// updated only when actor holds all of that user's permissions, the result
// may hold only permissions actor holds (ErrPermissionCeiling), and an
// enabled user with every permission must remain (ErrLastAdmin).
func (s *Service) Update(ctx context.Context, actor *identity.Identity, u *model.User) error {
	return s.save(ctx, u, func(tx storage.Storage, existing *model.User) error {
		if existing == nil {
			return storage.ErrNotFound
		}
		if err := CheckTarget(ctx, tx, actor, existing); err != nil {
			return err
		}
		if err := checkGrant(ctx, tx, actor, u); err != nil {
			return err
		}
		return keepAdmin(ctx, tx, existing, u)
	})
}

// Upsert creates or replaces a user without actor checks. It is for trusted
// internal callers such as bootstrap; request handlers use Create, Update,
// SetDisabled and Delete.
func (s *Service) Upsert(ctx context.Context, u *model.User) error {
	return s.save(ctx, u, nil)
}

// save normalizes and stores u. check, when set, runs inside the transaction
// with the stored user (nil when there is none) before the write.
func (s *Service) save(ctx context.Context, u *model.User, check func(tx storage.Storage, existing *model.User) error) error {
	if u == nil {
		return storage.ErrInvalidArgument
	}

	subject := strings.TrimSpace(strings.ToLower(u.Subject()))
	if subject == "" {
		return domain.ErrInvalidSubject
	}
	u.SetSubject(subject)
	u.SetName(strings.TrimSpace(u.Name()))

	email := strings.TrimSpace(strings.ToLower(u.Email()))
	if email != "" && !isValidEmail(email) {
		return domain.ErrInvalidEmail
	}
	u.SetEmail(email)

	err := s.store.WithTx(ctx, func(tx storage.Storage) error {
		switch existing, err := tx.GetUserBySubject(ctx, subject); {
		case err == nil:
			if existing.ID() != u.ID() {
				return storage.ErrAlreadyExists
			}
		case !errors.Is(err, storage.ErrNotFound):
			return err
		}
		if ids := u.RoleIDsAll(); len(ids) > 0 {
			if _, err := tx.GetRoles(ctx, ids); err != nil {
				return err
			}
		}
		if check != nil {
			existing, err := tx.GetUser(ctx, u.ID())
			switch {
			case errors.Is(err, storage.ErrNotFound):
				existing = nil
			case err != nil:
				return err
			}
			if err = check(tx, existing); err != nil {
				return err
			}
		}
		return tx.UpsertUser(ctx, u)
	})
	if err != nil {
		return err
	}

	s.logger.Debug().
		Str("user_id", u.ID()).
		Str("subject", subject).
		Msg("user upserted")
	return nil
}

// isValidEmail performs a basic email format check:
func isValidEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	if at < 1 || at == len(email)-1 {
		return false
	}
	d := email[at+1:]
	return strings.ContainsRune(d, '.') && !strings.HasSuffix(d, ".")
}
