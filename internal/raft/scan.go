package raft

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

var _ storage.Scanner = (*Store)(nil)

// Scans read the local state, as every read of a Raft Store does, and take the
// single pass of the inner store when it is a Scanner. The view WithTx hands out
// is not a Scanner: its listings bypass its own writes except for executions, and
// a scan through it pages as before.

func (s *Store) ScanAgents(ctx context.Context, filter storage.AgentFilter) ([]*model.Agent, error) {
	return storage.AllAgents(ctx, s.inner, filter)
}

func (s *Store) ScanRollouts(ctx context.Context, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	return storage.AllRollouts(ctx, s.inner, filter)
}

func (s *Store) ScanSpecs(ctx context.Context, filter storage.SpecFilter) ([]*model.Spec, error) {
	return storage.AllSpecs(ctx, s.inner, filter)
}

func (s *Store) ScanExecutions(ctx context.Context, filter storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	return storage.AllExecutions(ctx, s.inner, filter)
}
