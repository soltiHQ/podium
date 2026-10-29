package raft

import (
	"context"
	"errors"
	"fmt"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
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

func (v *txView) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	if e == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p := e.Params()
	_, err := v.GetExecution(ctx, e.ID())
	if err == nil {
		return nil // Replay of an archived identity never rewrites its terminal result.
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	next, err := model.NewExecutionRecord(p)
	if err != nil {
		return fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	if next.ID() != e.ID() {
		return storage.ErrInvalidArgument
	}
	v.executions[e.ID()] = next
	v.ops = append(v.ops, &raftv1.Op{Op: &raftv1.Op_ExecutionUpsert{ExecutionUpsert: wire.ExecutionToProto(next)}})
	return nil
}

func (v *txView) GetExecution(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if val, found, tomb := overlayGet(v.executions, id); found {
		if tomb {
			return nil, storage.ErrNotFound
		}
		return val.Clone(), nil
	}
	return v.inner.GetExecution(ctx, id)
}

// Archive retention must see pending inserts and deletes in the same atomic
// batch. Reuse the canonical backend pagination/filter implementation after
// merging its complete current listing with the transaction's bounded overlay.
func (v *txView) ListExecutions(ctx context.Context, f storage.ExecutionFilter, o storage.ListOptions) (*storage.ExecutionListResult, error) {
	rows, err := storage.AllExecutions(ctx, v.inner, nil)
	if err != nil {
		return nil, err
	}
	merged := inmemory.New()
	for _, e := range rows {
		if err := merged.UpsertExecution(ctx, e); err != nil {
			return nil, err
		}
	}
	for id, e := range v.executions {
		if e == nil {
			if err := merged.DeleteExecution(ctx, id); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return nil, err
			}
		} else if err := merged.UpsertExecution(ctx, e); err != nil {
			return nil, err
		}
	}
	return merged.ListExecutions(ctx, f, o)
}

func (v *txView) DeleteExecution(ctx context.Context, id string) error {
	if _, err := v.GetExecution(ctx, id); err != nil {
		return err
	}
	v.executions[id] = nil
	v.ops = append(v.ops, &raftv1.Op{Op: &raftv1.Op_ExecutionDelete{ExecutionDelete: id}})
	return nil
}

func (v *txView) BuildExecutionFilter(c storage.ExecutionQueryCriteria) storage.ExecutionFilter {
	return v.inner.BuildExecutionFilter(c)
}
