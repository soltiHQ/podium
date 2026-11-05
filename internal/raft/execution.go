package raft

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func (s *Store) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertExecution(ctx, e) })
}

func (s *Store) GetExecution(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	return s.inner.GetExecution(ctx, id)
}

func (s *Store) ListExecutions(ctx context.Context, f storage.ExecutionFilter, o storage.ListOptions) (*storage.ExecutionListResult, error) {
	return s.inner.ListExecutions(ctx, f, o)
}

func (s *Store) DeleteExecution(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteExecution(ctx, id) })
}

func (s *Store) BuildExecutionFilter(c storage.ExecutionQueryCriteria) storage.ExecutionFilter {
	return s.inner.BuildExecutionFilter(c)
}
