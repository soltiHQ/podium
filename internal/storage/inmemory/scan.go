package inmemory

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

var (
	_ storage.Scanner = (*Store)(nil)
	_ storage.Scanner = (*state)(nil)
)

// The scans of the Store hold the store-wide gate for the pass, as a listing
// does for a page; those of the transaction handle, state, do not. Each fails
// on a done ctx before it looks at the filter, as the first page of a drained
// listing does, whether the store holds rows or not.

func (s *Store) ScanAgents(ctx context.Context, filter storage.AgentFilter) ([]*model.Agent, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ScanAgents(ctx, filter)
}

func (s *Store) ScanRollouts(ctx context.Context, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ScanRollouts(ctx, filter)
}

func (s *Store) ScanSpecs(ctx context.Context, filter storage.SpecFilter) ([]*model.Spec, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ScanSpecs(ctx, filter)
}

func (s *Store) ScanExecutions(ctx context.Context, filter storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.ScanExecutions(ctx, filter)
}

func (s *state) ScanAgents(ctx context.Context, filter storage.AgentFilter) ([]*model.Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pred, err := agentPredicate(filter)
	if err != nil {
		return nil, err
	}
	return s.agents.all(ctx, pred, nil)
}

func (s *state) ScanRollouts(ctx context.Context, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pred, err := rolloutPredicate(filter)
	if err != nil {
		return nil, err
	}
	return s.rollouts.all(ctx, pred, nil)
}

func (s *state) ScanSpecs(ctx context.Context, filter storage.SpecFilter) ([]*model.Spec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pred, err := specPredicate(filter)
	if err != nil {
		return nil, err
	}
	return s.specs.all(ctx, pred, nil)
}

func (s *state) ScanExecutions(ctx context.Context, filter storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pred, err := executionPredicate(filter)
	if err != nil {
		return nil, err
	}
	return s.executions.all(ctx, pred, nil)
}
