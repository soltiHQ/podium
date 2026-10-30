package inmemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

type ExecutionFilter struct {
	criteria storage.ExecutionQueryCriteria
}

func (f *ExecutionFilter) Matches(e *model.ExecutionRecord) bool {
	c := f.criteria
	if c.AgentID != "" && e.AgentID() != c.AgentID || c.SpecID != "" && e.SpecID() != c.SpecID || c.TaskName != "" && e.TaskName() != c.TaskName || c.TaskUID != "" && e.TaskUID() != c.TaskUID {
		return false
	}
	if len(c.Statuses) > 0 {
		for _, s := range c.Statuses {
			if e.Status() == s {
				return true
			}
		}
		return false
	}
	return true
}

// executionPredicate resolves the filter of a listing or a scan to its predicate.
// A nil filter accepts every record; one built by another backend is rejected.
func executionPredicate(filter storage.ExecutionFilter) (func(*model.ExecutionRecord) bool, error) {
	if filter == nil {
		return nil, nil
	}
	typed, ok := filter.(*ExecutionFilter)
	if !ok || typed == nil {
		return nil, storage.ErrInvalidArgument
	}
	return typed.Matches, nil
}

func (s *state) BuildExecutionFilter(c storage.ExecutionQueryCriteria) storage.ExecutionFilter {
	c.Statuses = append([]string(nil), c.Statuses...)
	return &ExecutionFilter{criteria: c}
}

func (s *state) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return storage.ErrInvalidArgument
	}
	p := e.Params()
	_, err := s.executions.Get(ctx, e.ID())
	if err == nil {
		return nil // The first terminal result is immutable, including its capture time.
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
	return s.executions.Upsert(ctx, next)
}

func (s *state) GetExecution(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.executions.Get(ctx, id)
}

func (s *state) ListExecutions(ctx context.Context, f storage.ExecutionFilter, o storage.ListOptions) (*storage.ExecutionListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pred, err := executionPredicate(f)
	if err != nil {
		return nil, err
	}
	return s.executions.List(ctx, pred, o)
}

func (s *state) DeleteExecution(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.executions.Delete(ctx, id)
}

func (s *Store) UpsertExecution(ctx context.Context, e *model.ExecutionRecord) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.UpsertExecution(ctx, e)
}

func (s *Store) GetExecution(ctx context.Context, id string) (*model.ExecutionRecord, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.GetExecution(ctx, id)
}

func (s *Store) ListExecutions(ctx context.Context, f storage.ExecutionFilter, o storage.ListOptions) (*storage.ExecutionListResult, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ListExecutions(ctx, f, o)
}

func (s *Store) DeleteExecution(ctx context.Context, id string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.state.DeleteExecution(ctx, id)
}
