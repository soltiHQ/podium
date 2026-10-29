package durable

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func (d *Store) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertExecution(ctx, e) })
}

func (d *Store) GetExecution(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*model.ExecutionRecord, error) { return inner.GetExecution(ctx, id) })
}

func (d *Store) ListExecutions(ctx context.Context, f storage.ExecutionFilter, o storage.ListOptions) (*storage.ExecutionListResult, error) {
	return readState(ctx, d, func(inner *inmemory.Store) (*storage.ExecutionListResult, error) {
		return inner.ListExecutions(ctx, f, o)
	})
}

func (d *Store) DeleteExecution(ctx context.Context, id string) error {
	return d.WithTx(ctx, func(tx storage.Storage) error { return tx.DeleteExecution(ctx, id) })
}

func (d *Store) BuildExecutionFilter(c storage.ExecutionQueryCriteria) storage.ExecutionFilter {
	return d.filters.BuildExecutionFilter(c)
}
