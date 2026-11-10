package raft

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

var _ storage.Scanner = (*Store)(nil)

// Scans read the local state, as every read of a Raft Store does, and take the
// single pass of the inner store when it is a Scanner. The overlay.Tx WithTx hands
// out is a Scanner too: its scans make that pass and add the transaction's writes.

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

var _ storage.Counter = (*Store)(nil)

// Counts read the local state too, from the inner store's own count when it is a
// Counter.

func (s *Store) CountAgents(ctx context.Context) (map[enum.AgentStatus]int, error) {
	return storage.CountAgents(ctx, s.inner)
}

func (s *Store) CountRollouts(ctx context.Context) (map[enum.SyncStatus]int, error) {
	return storage.CountRollouts(ctx, s.inner)
}
