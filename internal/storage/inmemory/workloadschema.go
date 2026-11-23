package inmemory

import (
	"context"
	"errors"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func (s *state) UpsertWorkloadSchema(ctx context.Context, w *model.WorkloadSchema) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return storage.ErrInvalidArgument
	}
	next, err := model.NewWorkloadSchema(w.ID(), w.APIVersion(), w.Kind(), w.Schema(), w.FirstAgentID(), w.FirstSeenAt())
	if err != nil {
		return errors.Join(storage.ErrInvalidArgument, err)
	}
	if _, err := s.workloadSchemas.Get(ctx, next.ID()); err == nil {
		return nil // The first record of a fingerprint is immutable.
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return s.workloadSchemas.Upsert(ctx, next)
}

func (s *state) GetWorkloadSchema(ctx context.Context, fingerprint string) (*model.WorkloadSchema, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.workloadSchemas.Get(ctx, fingerprint)
}

func (s *state) ListWorkloadSchemas(ctx context.Context, o storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.workloadSchemas.List(ctx, nil, o)
}

func (s *Store) UpsertWorkloadSchema(ctx context.Context, w *model.WorkloadSchema) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertWorkloadSchema(ctx, w)
}

func (s *Store) GetWorkloadSchema(ctx context.Context, fingerprint string) (*model.WorkloadSchema, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetWorkloadSchema(ctx, fingerprint)
}

func (s *Store) ListWorkloadSchemas(ctx context.Context, o storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListWorkloadSchemas(ctx, o)
}
