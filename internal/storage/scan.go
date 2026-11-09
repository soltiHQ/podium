package storage

import (
	"context"
	"fmt"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
)

// CollectPages drains an opaque-cursor listing before callers mutate its rows.
// Rejecting repeated cursors prevents a faulty backend from wedging a runner.
// A store that holds its rows in process re-reads all of them for each page, so
// it should be a Scanner; the All* helpers prefer that to this.
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

// AllRollouts returns every rollout the filter matches, in the order of ListOptions:
// in one pass if the store is a Scanner, page by page otherwise.
func AllRollouts(ctx context.Context, s RolloutStore, filter RolloutFilter) ([]*model.Rollout, error) {
	if sc, ok := s.(Scanner); ok {
		return sc.ScanRollouts(ctx, filter)
	}
	return CollectPages(ctx, func(o ListOptions) (*RolloutListResult, error) { return s.ListRollouts(ctx, filter, o) })
}

// AllSpecs returns every spec the filter matches, as AllRollouts does rollouts.
func AllSpecs(ctx context.Context, s SpecStore, filter SpecFilter) ([]*model.Spec, error) {
	if sc, ok := s.(Scanner); ok {
		return sc.ScanSpecs(ctx, filter)
	}
	return CollectPages(ctx, func(o ListOptions) (*SpecListResult, error) { return s.ListSpecs(ctx, filter, o) })
}

// AllAgents returns every agent the filter matches, as AllRollouts does rollouts.
func AllAgents(ctx context.Context, s AgentStore, filter AgentFilter) ([]*model.Agent, error) {
	if sc, ok := s.(Scanner); ok {
		return sc.ScanAgents(ctx, filter)
	}
	return CollectPages(ctx, func(o ListOptions) (*AgentListResult, error) { return s.ListAgents(ctx, filter, o) })
}

// CountAgents returns how many agents have each status: from the store's own
// count when it is a Counter, from a scan of the rows otherwise.
func CountAgents(ctx context.Context, s AgentStore) (map[enum.AgentStatus]int, error) {
	if c, ok := s.(Counter); ok {
		return c.CountAgents(ctx)
	}
	agents, err := AllAgents(ctx, s, nil)
	if err != nil {
		return nil, err
	}
	counts := make(map[enum.AgentStatus]int)
	for _, a := range agents {
		counts[a.Status()]++
	}
	return counts, nil
}

// CountRollouts returns how many rollouts have each sync status, as CountAgents
// counts agents.
func CountRollouts(ctx context.Context, s RolloutStore) (map[enum.SyncStatus]int, error) {
	if c, ok := s.(Counter); ok {
		return c.CountRollouts(ctx)
	}
	rollouts, err := AllRollouts(ctx, s, nil)
	if err != nil {
		return nil, err
	}
	counts := make(map[enum.SyncStatus]int)
	for _, r := range rollouts {
		counts[r.Status()]++
	}
	return counts, nil
}
