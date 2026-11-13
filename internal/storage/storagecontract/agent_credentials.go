package storagecontract

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// agentCredentials checks the agent credential rows, keyed by agent ID: the
// round trip of both kinds, replacement, the rows a store refuses and deletion.
func agentCredentials(t *testing.T, newStore storeFunc) {
	t.Run("round trip", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		// A stored and a derived credential together set every field of the model.
		stored := storedAgentCredential(t, "agent-a", 0, minute(1))
		derived := derivedAgentCredential(t, "agent-b", 0, minute(0))
		must(t, errors.Join(s.UpsertAgentCredential(ctx, stored), s.UpsertAgentCredential(ctx, derived)))

		for _, want := range []*model.AgentCredential{stored, derived} {
			got, err := s.GetAgentCredential(ctx, want.ID())
			gotRow(t, "GetAgentCredential", got, err, want, equalAgentCredential)
		}
		listed, err := listedAgentCredentials(ctx, s)
		must(t, err)
		equalRows(t, "ListAgentCredentials", listed, []*model.AgentCredential{stored, derived}, equalAgentCredential)
	})

	t.Run("upsert replaces", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		other := storedAgentCredential(t, "agent-b", 0, minute(3))
		must(t, errors.Join(s.UpsertAgentCredential(ctx, storedAgentCredential(t, "agent-a", 0, minute(0))), s.UpsertAgentCredential(ctx, other)))

		// A reset replaces a stored credential by a derived one: every field
		// changes, the creation time too, and the row lists at its new one.
		want := derivedAgentCredential(t, "agent-a", 1, minute(5))
		must(t, s.UpsertAgentCredential(ctx, want))
		got, err := s.GetAgentCredential(ctx, want.ID())
		gotRow(t, "GetAgentCredential", got, err, want, equalAgentCredential)
		listed, err := listedAgentCredentials(ctx, s)
		must(t, err)
		equalRows(t, "ListAgentCredentials", listed, []*model.AgentCredential{want, other}, equalAgentCredential)
	})

	t.Run("invalid rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		noID := &model.AgentCredential{}
		noID.SetCreatedAt(minute(0))
		noCreation := storedAgentCredential(t, "agent-a", 0, minute(0))
		noCreation.SetCreatedAt(time.Time{})
		expect(t,
			outcome{"UpsertAgentCredential of nil", s.UpsertAgentCredential(ctx, nil), storage.ErrInvalidArgument},
			outcome{"UpsertAgentCredential of a row without an ID", s.UpsertAgentCredential(ctx, noID), storage.ErrInvalidArgument},
			outcome{"UpsertAgentCredential of a row without a creation time", s.UpsertAgentCredential(ctx, noCreation), storage.ErrInvalidArgument},
		)
		listed, err := listedAgentCredentials(ctx, s)
		sameIDs(t, "ListAgentCredentials after the refused writes", listed, err)
	})

	t.Run("delete and missing rows", func(t *testing.T) {
		s, ctx := newStore(t), t.Context()
		kept := derivedAgentCredential(t, "agent-b", 0, minute(0))
		must(t, errors.Join(s.UpsertAgentCredential(ctx, storedAgentCredential(t, "agent-a", 0, minute(0))), s.UpsertAgentCredential(ctx, kept)))
		expect(t,
			outcome{"DeleteAgentCredential", s.DeleteAgentCredential(ctx, "agent-a"), nil},
			outcome{"GetAgentCredential of a deleted row", errOf(s.GetAgentCredential(ctx, "agent-a")), storage.ErrNotFound},
			outcome{"DeleteAgentCredential of a deleted row", s.DeleteAgentCredential(ctx, "agent-a"), storage.ErrNotFound},
			outcome{"GetAgentCredential of an ID never written", errOf(s.GetAgentCredential(ctx, "agent-x")), storage.ErrNotFound},
			outcome{"DeleteAgentCredential of an ID never written", s.DeleteAgentCredential(ctx, "agent-x"), storage.ErrNotFound},
			outcome{"GetAgentCredential of an empty ID", errOf(s.GetAgentCredential(ctx, "")), storage.ErrInvalidArgument},
			outcome{"DeleteAgentCredential of an empty ID", s.DeleteAgentCredential(ctx, ""), storage.ErrInvalidArgument},
		)
		got, err := s.GetAgentCredential(ctx, kept.ID())
		gotRow(t, "GetAgentCredential of the row kept", got, err, kept, equalAgentCredential)
	})
}

// listedAgentCredentials drains ListAgentCredentials.
func listedAgentCredentials(ctx context.Context, s storage.Storage) ([]*model.AgentCredential, error) {
	return storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.AgentCredentialListResult, error) {
		return s.ListAgentCredentials(ctx, o)
	})
}

// storedAgentCredential is the stored credential of agentID, which holds a
// token, with every field set from v.
func storedAgentCredential(t *testing.T, agentID string, v int, created time.Time) *model.AgentCredential {
	t.Helper()
	c, err := model.NewStoredAgentCredential(agentID, fmt.Sprintf("token-%s-%d", agentID, v))
	if err != nil {
		t.Fatal(err)
	}
	c.SetCreatedAt(created)
	c.SetUpdatedAt(created.Add(time.Duration(v+1) * time.Minute))
	return c
}

// derivedAgentCredential is the derived credential of agentID, which holds a
// generation and a secret ID, with every field set from v.
func derivedAgentCredential(t *testing.T, agentID string, v int, created time.Time) *model.AgentCredential {
	t.Helper()
	c, err := model.NewDerivedAgentCredential(agentID, uint32(7+v), []byte{0xa0, byte(v), 0x5c})
	if err != nil {
		t.Fatal(err)
	}
	c.SetCreatedAt(created)
	c.SetUpdatedAt(created.Add(time.Duration(v+1) * time.Minute))
	return c
}

// equalAgentCredential compares every accessor of an agent credential read back
// with the one written.
func equalAgentCredential(t *testing.T, what string, got, want *model.AgentCredential) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil agent credential, want %s", what, want.ID())
		return
	}
	f := fields{t, what}
	f.same("ID", got.ID(), want.ID())
	f.same("AgentID", got.AgentID(), want.AgentID())
	f.same("Derived", got.Derived(), want.Derived())
	f.same("Generation", got.Generation(), want.Generation())
	f.same("SecretID", got.SecretID(), want.SecretID())
	f.same("Token", got.Token(), want.Token())
	f.instant("CreatedAt", got.CreatedAt(), want.CreatedAt())
	f.instant("UpdatedAt", got.UpdatedAt(), want.UpdatedAt())
}
