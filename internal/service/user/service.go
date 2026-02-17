package user

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
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

	out := make([]*model.User, 0, len(res.Items))
	for _, u := range res.Items {
		if u == nil {
			continue
		}
		out = append(out, u.Clone())
	}
	return &Page{Items: out, NextCursor: res.NextCursor}, nil
}

// Get returns a single user by ID.
func (s *Service) Get(ctx context.Context, id string) (*model.User, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}
	u, err := s.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, storage.ErrInternal
	}
	return u.Clone(), nil
}

// GetBySubject returns a single user by authentication subject.
func (s *Service) GetBySubject(ctx context.Context, subject string) (*model.User, error) {
	if subject == "" {
		return nil, storage.ErrInvalidArgument
	}
	u, err := s.store.GetUserBySubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, storage.ErrInternal
	}
	return u.Clone(), nil
}

// Delete a user by ID.
func (s *Service) Delete(ctx context.Context, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}

	if err := s.store.DeleteSessionsByUser(ctx, id); err != nil {
		return err
	}

	creds, err := s.store.ListCredentialsByUser(ctx, id)
	if err != nil {
		return err
	}
	for _, c := range creds {
		if c == nil {
			continue
		}

		if err = s.store.DeleteVerifierByCredential(ctx, c.ID()); err != nil {
			return err
		}
		if err = s.store.DeleteCredential(ctx, c.ID()); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	if err = s.store.DeleteUser(ctx, id); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return nil
}

// Upsert a user.
// TODO: before insert check if role exist
func (s *Service) Upsert(ctx context.Context, u *model.User) error {
	return s.store.UpsertUser(ctx, u)
}
