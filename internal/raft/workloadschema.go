package raft

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func (s *Store) UpsertWorkloadSchema(ctx context.Context, w *model.WorkloadSchema) error {
	return s.WithTx(ctx, func(tx storage.Storage) error { return tx.UpsertWorkloadSchema(ctx, w) })
}

func (s *Store) GetWorkloadSchema(ctx context.Context, fingerprint string) (*model.WorkloadSchema, error) {
	return s.inner.GetWorkloadSchema(ctx, fingerprint)
}

func (s *Store) ListWorkloadSchemas(ctx context.Context, o storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
	return s.inner.ListWorkloadSchemas(ctx, o)
}
