package durable

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

var _ storage.Scanner = (*Store)(nil)

// A scan reads the published state under one read lock, like any other read, where
// draining a listing takes it again for every page. Its rows are one state, and
// the apply of a committed transaction waits for it once.

func (d *Store) ScanAgents(ctx context.Context, filter storage.AgentFilter) ([]*model.Agent, error) {
	return readState(ctx, d, func(inner *inmemory.Store) ([]*model.Agent, error) { return inner.ScanAgents(ctx, filter) })
}

func (d *Store) ScanRollouts(ctx context.Context, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	return readState(ctx, d, func(inner *inmemory.Store) ([]*model.Rollout, error) { return inner.ScanRollouts(ctx, filter) })
}

func (d *Store) ScanSpecs(ctx context.Context, filter storage.SpecFilter) ([]*model.Spec, error) {
	return readState(ctx, d, func(inner *inmemory.Store) ([]*model.Spec, error) { return inner.ScanSpecs(ctx, filter) })
}

func (d *Store) ScanExecutions(ctx context.Context, filter storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	return readState(ctx, d, func(inner *inmemory.Store) ([]*model.ExecutionRecord, error) {
		return inner.ScanExecutions(ctx, filter)
	})
}
