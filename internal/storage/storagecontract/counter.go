package storagecontract

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// counter checks the optional storage.Counter: the agents by status and the
// rollouts by sync status, a status no row has absent, a done context refused,
// and in a transaction the transaction counted.
func counter(t *testing.T, newStore storeFunc) {
	if _, ok := newStore(t).(storage.Counter); !ok {
		t.Skip("the store does not implement storage.Counter")
	}

	t.Run("counts by status", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		for i, status := range []enum.AgentStatus{enum.AgentStatusActive, enum.AgentStatusDisconnected, enum.AgentStatusActive} {
			a := fullAgent(t, fmt.Sprintf("agent-%d", i), 0, minute(i))
			a.SetStatus(status)
			must(t, s.UpsertAgent(ctx, a))
		}
		for i, status := range []enum.SyncStatus{enum.SyncStatusSynced, enum.SyncStatusFailed, enum.SyncStatusSynced, enum.SyncStatusPending} {
			must(t, s.UpsertRollout(ctx, rolloutIn(t, fmt.Sprintf("spec-%d", i), "agent-0", status)))
		}
		c := counterOf(t, s)
		agents, err := c.CountAgents(ctx)
		sameCounts(t, "CountAgents", agents, err, map[enum.AgentStatus]int{enum.AgentStatusActive: 2, enum.AgentStatusDisconnected: 1})
		rollouts, err := c.CountRollouts(ctx)
		sameCounts(t, "CountRollouts", rollouts, err, map[enum.SyncStatus]int{enum.SyncStatusSynced: 2, enum.SyncStatusFailed: 1, enum.SyncStatusPending: 1})
	})

	t.Run("empty store", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		c := counterOf(t, s)
		agents, err := c.CountAgents(ctx)
		sameCounts(t, "CountAgents", agents, err, map[enum.AgentStatus]int{})
		rollouts, err := c.CountRollouts(ctx)
		sameCounts(t, "CountRollouts", rollouts, err, map[enum.SyncStatus]int{})
	})

	t.Run("done context", func(t *testing.T) {
		s := newStore(t)
		c, done := counterOf(t, s), doneContext(t)
		expect(t,
			outcome{"CountAgents with a done context", errOf(c.CountAgents(done)), context.Canceled},
			outcome{"CountRollouts with a done context", errOf(c.CountRollouts(done)), context.Canceled},
		)
	})

	t.Run("the transaction handle counts the transaction", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		active := fullAgent(t, "agent-a", 0, minute(0))
		active.SetStatus(enum.AgentStatusActive)
		synced := rolloutIn(t, "spec-a", "agent-a", enum.SyncStatusSynced)
		must(t, errors.Join(s.UpsertAgent(ctx, active), s.UpsertRollout(ctx, synced)))

		err := s.WithTx(ctx, func(tx storage.Storage) error {
			c, ok := tx.(storage.Counter)
			if !ok {
				return errors.New("the transaction handle does not implement storage.Counter, as the store does")
			}
			if err := errors.Join(
				tx.DeleteAgent(ctx, active.ID()),
				tx.UpsertAgent(ctx, fullAgent(t, "agent-b", 1, minute(1))),
				tx.DeleteRollout(ctx, synced.ID()),
				tx.UpsertRollout(ctx, rolloutIn(t, "spec-b", "agent-b", enum.SyncStatusPending)),
			); err != nil {
				return err
			}
			agents, err := c.CountAgents(ctx)
			sameCounts(t, "CountAgents inside the transaction", agents, err, map[enum.AgentStatus]int{enum.AgentStatusDisconnected: 1})
			rollouts, err := c.CountRollouts(ctx)
			sameCounts(t, "CountRollouts inside the transaction", rollouts, err, map[enum.SyncStatus]int{enum.SyncStatusPending: 1})
			return errRollback
		})
		expect(t, outcome{"WithTx", err, errRollback})
	})
}

// counterOf returns s as the storage.Counter the group's first store was.
func counterOf(t *testing.T, s storage.Storage) storage.Counter {
	t.Helper()
	c, ok := s.(storage.Counter)
	if !ok {
		t.Fatal("the store does not implement storage.Counter, as the first store of the group did")
	}
	return c
}

// sameCounts fails t unless a count returned exactly want. The spec is silent
// on a count of no row: the in-memory store, and the scan storage.CountAgents
// falls back to, return an empty map, not nil.
func sameCounts[K comparable](t *testing.T, what string, got map[K]int, err error, want map[K]int) {
	t.Helper()
	switch {
	case err != nil:
		t.Errorf("%s: %v", what, err)
	case got == nil:
		t.Errorf("%s: nil, want %v", what, want)
	case !maps.Equal(got, want):
		t.Errorf("%s: %v, want %v", what, got, want)
	}
}
