package sqlstore

import (
	"context"
	"fmt"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// An execution record's row holds the identity of its run, its spec and its
// status beside the body, for the criteria; its creation time is its capture
// time.

var (
	executionKind   = kind[*model.ExecutionRecord]{"podium_executions", snapshot.UnmarshalExecutionRecordJSON}
	insertExecution = insertIfAbsent("podium_executions", "created_at", "agent_id", "spec_id", "task_name", "task_uid", "status", "body")
)

// UpsertExecution records a terminal result: it validates the record as the model
// does, then inserts it only when no record has its ID. The first record of a run
// is immutable, so a replay changes nothing and is no error.
func (w *writer) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	if e == nil {
		return storage.ErrInvalidArgument
	}
	next, err := model.NewExecutionRecord(e.Params())
	if err != nil {
		return fmt.Errorf("%w: sqlstore: the execution record: %v", storage.ErrInvalidArgument, err)
	}
	if next.ID() != e.ID() {
		return fmt.Errorf("%w: sqlstore: the execution record's ID is not its run's", storage.ErrInvalidArgument)
	}
	body, err := snapshot.MarshalExecutionRecordJSON(next)
	if err != nil {
		return err
	}
	id, err := indexKey("execution record ID", next.ID())
	if err != nil {
		return err
	}
	agent, err := indexKey("execution record's agent ID", next.AgentID())
	if err != nil {
		return err
	}
	spec, err := indexKey("execution record's spec ID", next.SpecID())
	if err != nil {
		return err
	}
	created, err := columnTime("capture time", next.CreatedAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, insertExecution, id, created, agent, spec, bin(next.TaskName()), bin(next.TaskUID()), next.Status(), body)
	return err
}

// GetExecution returns the execution record with the ID.
func (r *reader) GetExecution(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	return byID(ctx, r, executionKind, id)
}

// ListExecutions returns a page of the execution records the filter matches.
func (r *reader) ListExecutions(ctx context.Context, filter storage.ExecutionFilter, opts storage.ListOptions) (*storage.ExecutionListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, err := executionQuery(filter)
	if err != nil {
		return nil, err
	}
	return listPage(ctx, r, q, opts)
}

// DeleteExecution deletes the execution record with the ID.
func (w *writer) DeleteExecution(ctx context.Context, id string) error {
	return w.deleteByID(ctx, executionKind.table, id)
}

// UpsertExecution records a terminal result, in a write transaction of its own.
func (s *Store) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertExecution(ctx, e) })
}

// ListExecutions returns a page of the execution records the filter matches,
// from the replica for a read-only context.
func (s *Store) ListExecutions(ctx context.Context, filter storage.ExecutionFilter, opts storage.ListOptions) (*storage.ExecutionListResult, error) {
	return routed(ctx, s, func(r *reader) (*storage.ExecutionListResult, error) { return r.ListExecutions(ctx, filter, opts) })
}

// DeleteExecution deletes the execution record with the ID, in a write
// transaction of its own.
func (s *Store) DeleteExecution(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteExecution(ctx, id) })
}
