package inmemory

import (
	"context"
	"sync"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// txMu is the per-Store write lock for WithTx, kept out-of-struct so the
// Store definition stays untouched. One transaction at a time per Store.
var txMu sync.Map // map[*Store]*sync.Mutex

func (s *Store) txLock() *sync.Mutex {
	if m, ok := txMu.Load(s); ok {
		return m.(*sync.Mutex)
	}
	m, _ := txMu.LoadOrStore(s, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// snapshot is a shallow copy of every GenericStore's data map. Values are
// already stored as clones by the write path, so a map-level copy is
// sufficient for rollback.
type snapshot struct {
	agents      map[string]*model.Agent
	users       map[string]*model.User
	roles       map[string]*model.Role
	credentials map[string]*model.Credential
	verifiers   map[string]*model.Verifier
	sessions    map[string]*model.Session
	specs       map[string]*model.Spec
	rollouts    map[string]*model.Rollout
}

func (s *Store) takeSnapshot() snapshot {
	return snapshot{
		agents:      copyMap(s.agents),
		users:       copyMap(s.users),
		roles:       copyMap(s.roles),
		credentials: copyMap(s.credentials),
		verifiers:   copyMap(s.verifiers),
		sessions:    copyMap(s.sessions),
		specs:       copyMap(s.specs),
		rollouts:    copyMap(s.rollouts),
	}
}

func (s *Store) restoreSnapshot(snap snapshot) {
	restoreMap(s.agents, snap.agents)
	restoreMap(s.users, snap.users)
	restoreMap(s.roles, snap.roles)
	restoreMap(s.credentials, snap.credentials)
	restoreMap(s.verifiers, snap.verifiers)
	restoreMap(s.sessions, snap.sessions)
	restoreMap(s.specs, snap.specs)
	restoreMap(s.rollouts, snap.rollouts)
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

// WithTx serialises writes and runs fn as an atomic transaction.
//
// On nil return from fn: mutations are committed (they were already in
// place, since the in-memory Store mutates directly).
// On error: the pre-transaction snapshot is restored and the error is
// propagated.
//
// Nested WithTx inside fn calls fn without re-locking (already-inside-a-tx
// semantics) so helpers that defensively call WithTx can be composed.
func (s *Store) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	lock := s.txLock()
	lock.Lock()
	defer lock.Unlock()

	snap := s.takeSnapshot()
	if err := fn(&txView{Store: s}); err != nil {
		s.restoreSnapshot(snap)
		return err
	}
	return nil
}

// txView wraps *Store so nested WithTx calls inside fn don't re-lock.
type txView struct{ *Store }

func (t *txView) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(t)
}
