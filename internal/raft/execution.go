package raft

import (
	"context"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func (s *Store) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	if e == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.applyOp(&raftv1.Op{Op: &raftv1.Op_ExecutionUpsert{ExecutionUpsert: wire.ExecutionToProto(e)}})
}

func (s *Store) GetExecution(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	return s.inner.GetExecution(ctx, id)
}

func (s *Store) ListExecutions(ctx context.Context, f storage.ExecutionFilter, o storage.ListOptions) (*storage.ExecutionListResult, error) {
	return s.inner.ListExecutions(ctx, f, o)
}

func (s *Store) DeleteExecution(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		return storage.ErrInvalidArgument
	}
	return s.applyOp(&raftv1.Op{Op: &raftv1.Op_ExecutionDelete{ExecutionDelete: id}})
}

func (s *Store) BuildExecutionFilter(c storage.ExecutionQueryCriteria) storage.ExecutionFilter {
	return s.inner.BuildExecutionFilter(c)
}
