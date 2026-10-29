package storage

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
)

type (
	ExecutionListResult    = ListResult[*model.ExecutionRecord]
	ExecutionFilter        interface{}
	ExecutionQueryCriteria struct {
		AgentID, SpecID, TaskName, TaskUID string
		Statuses                           []string
	}
)

// ExecutionStore retains terminal observations without agent/spec cascades.
// Upsert is a no-op for an existing identity: the first complete terminal
// result and its CapturedAt are immutable. Conflicting replays do not replace it.
type ExecutionStore interface {
	UpsertExecution(context.Context, *model.ExecutionRecord) error
	GetExecution(context.Context, string) (*model.ExecutionRecord, error)
	ListExecutions(context.Context, ExecutionFilter, ListOptions) (*ExecutionListResult, error)
	DeleteExecution(context.Context, string) error
}

func AllExecutions(ctx context.Context, s ExecutionStore, f ExecutionFilter) ([]*model.ExecutionRecord, error) {
	return CollectPages(ctx, func(o ListOptions) (*ExecutionListResult, error) { return s.ListExecutions(ctx, f, o) })
}
