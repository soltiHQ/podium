package inmemory

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

var (
	_ storage.Counter = (*Store)(nil)
	_ storage.Counter = (*state)(nil)
)

// The counts of the Store hold the store-wide gate for the pass, as a scan does;
// those of the transaction handle, state, do not.

func (s *Store) CountAgents(ctx context.Context) (map[enum.AgentStatus]int, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.CountAgents(ctx)
}

func (s *Store) CountRollouts(ctx context.Context) (map[enum.SyncStatus]int, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.state.CountRollouts(ctx)
}

func (s *state) CountAgents(ctx context.Context) (map[enum.AgentStatus]int, error) {
	return countBy(ctx, s.agents, (*model.Agent).Status)
}

func (s *state) CountRollouts(ctx context.Context) (map[enum.SyncStatus]int, error) {
	return countBy(ctx, s.rollouts, (*model.Rollout).Status)
}
