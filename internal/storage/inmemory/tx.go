package inmemory

import (
	"context"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// snapshot is a shallow copy of every GenericStore's data map. Values are
// already stored as clones by the write path, so a map-level copy is
// sufficient for rollback.
type snapshot struct {
	agents      map[string]*model.Agent
	agentCreds  map[string]*model.AgentCredential
	users       map[string]*model.User
	roles       map[string]*model.Role
	credentials map[string]*model.Credential
	verifiers   map[string]*model.Verifier
	sessions    map[string]*model.Session
	specs       map[string]*model.Spec
	rollouts    map[string]*model.Rollout
	executions  map[string]*model.ExecutionRecord
}

func (s *state) takeSnapshot() snapshot {
	return snapshot{
		agents:      copyMap(s.agents),
		agentCreds:  copyMap(s.agentCreds),
		users:       copyMap(s.users),
		roles:       copyMap(s.roles),
		credentials: copyMap(s.credentials),
		verifiers:   copyMap(s.verifiers),
		sessions:    copyMap(s.sessions),
		specs:       copyMap(s.specs),
		rollouts:    copyMap(s.rollouts),
		executions:  copyMap(s.executions),
	}
}

func (s *state) restoreSnapshot(snap snapshot) {
	restoreMap(s.agents, snap.agents)
	restoreMap(s.agentCreds, snap.agentCreds)
	restoreMap(s.users, snap.users)
	restoreMap(s.roles, snap.roles)
	restoreMap(s.credentials, snap.credentials)
	restoreMap(s.verifiers, snap.verifiers)
	restoreMap(s.sessions, snap.sessions)
	restoreMap(s.specs, snap.specs)
	restoreMap(s.rollouts, snap.rollouts)
	restoreMap(s.executions, snap.executions)
}

func copyMap[T domain.Entity[T]](g *GenericStore[T]) map[string]T {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make(map[string]T, len(g.data))
	for k, v := range g.data {
		out[k] = v
	}
	return out
}

func restoreMap[T domain.Entity[T]](g *GenericStore[T], snap map[string]T) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.data = snap
}

// WithTx excludes every public read and write until the callback commits or
// rolls back. The callback receives ungated operations, so nested transactions
// compose without attempting to reacquire the gate. A cancelled callback is
// rolled back even when it returns nil.
func (s *Store) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	snap := s.state.takeSnapshot()
	committed := false
	defer func() {
		if !committed {
			s.state.restoreSnapshot(snap)
		}
	}()
	if err := fn(s.state); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *state) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(s)
}
