package storage

import (
	"context"
	"fmt"

	"github.com/soltiHQ/control-plane/domain/model"
)

// CollectPages drains an opaque-cursor listing before callers mutate its rows.
// Rejecting repeated cursors prevents a faulty backend from wedging a runner.
func CollectPages[T any](ctx context.Context, list func(ListOptions) (*ListResult[T], error)) ([]T, error) {
	var items []T
	seen := make(map[string]bool)
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := list(ListOptions{Limit: MaxListLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, fmt.Errorf("%w: nil list page", ErrInternal)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			return items, nil
		}
		if seen[page.NextCursor] {
			return nil, fmt.Errorf("%w: repeated list cursor", ErrInternal)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

func AllRollouts(ctx context.Context, s RolloutStore, filter RolloutFilter) ([]*model.Rollout, error) {
	return CollectPages(ctx, func(o ListOptions) (*RolloutListResult, error) { return s.ListRollouts(ctx, filter, o) })
}

func AllSpecs(ctx context.Context, s SpecStore, filter SpecFilter) ([]*model.Spec, error) {
	return CollectPages(ctx, func(o ListOptions) (*SpecListResult, error) { return s.ListSpecs(ctx, filter, o) })
}

func AllAgents(ctx context.Context, s AgentStore, filter AgentFilter) ([]*model.Agent, error) {
	return CollectPages(ctx, func(o ListOptions) (*AgentListResult, error) { return s.ListAgents(ctx, filter, o) })
}
