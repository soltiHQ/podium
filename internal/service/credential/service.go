// Package credential implements credential management use-cases:
//   - Listing credentials by user
//   - Credential retrieval and deletion (with verifier cascade)
//   - Password creation and replacement.
package credential

import (
	"context"
	"errors"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	authcred "github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/service"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Service provides credential management operations.
type Service struct {
	logger zerolog.Logger
	store  storage.Storage
}

// New creates a new credential service.
func New(store storage.Storage, logger zerolog.Logger) *Service {
	if store == nil {
		panic("credential.Service: store is nil")
	}
	return &Service{
		logger: logger.With().Str("service", "credentials").Logger(),
		store:  store,
	}
}

// ListByUser returns credentials bound to a user.
func (s *Service) ListByUser(ctx context.Context, q ListByUserQuery) (*Page, error) {
	if q.UserID == "" {
		return nil, storage.ErrInvalidArgument
	}

	items, err := s.store.ListCredentialsByUser(ctx, q.UserID)
	if err != nil {
		return nil, err
	}
	limit := service.NormalizeListLimit(q.Limit, defaultListLimit)
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	out := make([]*model.Credential, 0, len(items))
	for _, c := range items {
		if c == nil {
			continue
		}
		out = append(out, c.Clone())
	}
	return &Page{Items: out}, nil
}

// Get returns a single credential by ID.
func (s *Service) Get(ctx context.Context, id string) (*model.Credential, error) {
	if id == "" {
		return nil, storage.ErrInvalidArgument
	}

	c, err := s.store.GetCredential(ctx, id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, storage.ErrInternal
	}
	return c.Clone(), nil
}

// Delete removes a credential by ID and cascades verifier deletion.
func (s *Service) Delete(ctx context.Context, req DeleteRequest) error {
	if req.ID == "" {
		return storage.ErrInvalidArgument
	}

	if err := s.store.DeleteVerifierByCredential(ctx, req.ID); err != nil {
		return err
	}
	if err := s.store.DeleteCredential(ctx, req.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return nil
}

// SetPassword creates or replaces password auth material for a user.
func (s *Service) SetPassword(ctx context.Context, req SetPasswordRequest) error {
	if req.UserID == "" || req.VerifierID == "" || req.Password == "" {
		return auth.ErrInvalidRequest
	}

	u, err := s.store.GetUser(ctx, req.UserID)
	if err != nil {
		return err
	}
	if u == nil || u.Disabled() {
		return auth.ErrInvalidRequest
	}

	credID := req.CredentialID
	if credID == "" {
		existing, err := s.store.GetCredentialByUserAndAuth(ctx, req.UserID, kind.Password)
		if err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			// No password credential yet — generate a new ID.
			credID = "cred-" + req.UserID
		} else {
			credID = existing.ID()
		}
	}

	cred, err := model.NewCredential(credID, req.UserID, kind.Password)
	if err != nil {
		return storage.ErrInvalidArgument
	}
	if err = s.store.UpsertCredential(ctx, cred); err != nil {
		return err
	}

	ver, err := authcred.NewPasswordVerifier(req.VerifierID, credID, req.Password, req.Cost)
	if err != nil {
		return err
	}

	if err = s.store.DeleteVerifierByCredential(ctx, credID); err != nil {
		return err
	}
	if err = s.store.UpsertVerifier(ctx, ver); err != nil {
		return err
	}
	return nil
}
