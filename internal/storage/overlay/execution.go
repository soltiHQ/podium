package overlay

import (
	"context"
	"errors"
	"fmt"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// UpsertExecution keeps the first result of a run: rewriting a record the
// transaction sees records nothing, as the state machine would change nothing.
func (t *Tx) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return storage.ErrInvalidArgument
	}
	switch _, err := t.GetExecution(ctx, e.ID()); {
	case err == nil:
		return nil
	case !errors.Is(err, storage.ErrNotFound):
		return err
	}
	next, err := model.NewExecutionRecord(e.Params())
	if err != nil {
		return fmt.Errorf("%w: %w", storage.ErrInvalidArgument, err)
	}
	if next.ID() != e.ID() {
		return storage.ErrInvalidArgument
	}
	msg := wire.ExecutionToProto(next)
	row, err := wire.ExecutionFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.executions.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_ExecutionUpsert{ExecutionUpsert: msg}})
	return nil
}

func (t *Tx) GetExecution(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t.executions.get(ctx, id, t.base.GetExecution)
}

func (t *Tx) ListExecutions(ctx context.Context, filter storage.ExecutionFilter, opts storage.ListOptions) (*storage.ExecutionListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return list(opts, func() ([]*model.ExecutionRecord, error) { return t.ScanExecutions(ctx, filter) })
}

func (t *Tx) ScanExecutions(ctx context.Context, filter storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	return scan(&t.executions, filter, func() ([]*model.ExecutionRecord, error) {
		return storage.AllExecutions(ctx, t.base, filter)
	})
}

func (t *Tx) DeleteExecution(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.executions.remove(ctx, id, t.base.GetExecution); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_ExecutionDelete{ExecutionDelete: id}})
	return nil
}
