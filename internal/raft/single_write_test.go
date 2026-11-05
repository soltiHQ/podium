package raft

import (
	"context"
	"errors"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// commandsAfter returns how many entries of n's log after index after are
// commands, the entries that carry a write. The barrier that starts the first
// transaction of a leadership term, a no-op and a configuration are entries
// too, but carry none.
func commandsAfter(t *testing.T, n *loopbackNode, after uint64) int {
	t.Helper()
	last, err := n.profile.logStore.LastIndex()
	if err != nil {
		t.Fatal(err)
	}
	commands := 0
	for index := after + 1; index <= last; index++ {
		var entry hraft.Log
		if err := n.profile.logStore.GetLog(index, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type == hraft.LogCommand {
			commands++
		}
	}
	return commands
}

// A single-operation write is a transaction of one operation, as on the durable
// store: the overlay checks it against the state, as the FSM will check it when
// it applies it, before anything is proposed. So the delete of a row that is
// absent gives its caller ErrNotFound with no command in the log, and no replica
// has a failed command to log. A bulk delete that matches nothing is no write at
// all. A write that is valid still goes through the log, once.
func TestSingleWritesAreValidatedBeforeTheyAreProposed(t *testing.T) {
	c := loopbackNodes(t, "n1", "n2", "n3")
	logs := make(map[string]*logBuffer, len(c.nodes))
	for _, n := range c.nodes {
		logs[n.id] = n.capture()
		n.start(t, c.discovery(t))
	}
	leader := c.leader(t, 10*time.Second)
	store := leader.profile.Store()
	clocked, ok := store.(storage.ClockedSessionRotation)
	if !ok {
		t.Fatalf("%T has no explicit session rotation timestamps", store)
	}
	ctx, later := context.Background(), time.Now().Add(time.Hour)
	hash := []byte("hash")

	for _, tc := range []struct {
		name  string
		write func() error
		want  error // nil for a write that is no error, such as a bulk delete that matches nothing
	}{
		{"DeleteAgent", func() error { return store.DeleteAgent(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteAgentCredential", func() error { return store.DeleteAgentCredential(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteUser", func() error { return store.DeleteUser(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteRole", func() error { return store.DeleteRole(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteCredential", func() error { return store.DeleteCredential(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteVerifier", func() error { return store.DeleteVerifier(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteSession", func() error { return store.DeleteSession(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteSpec", func() error { return store.DeleteSpec(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteRollout", func() error { return store.DeleteRollout(ctx, "absent") }, storage.ErrNotFound},
		{"DeleteExecution", func() error { return store.DeleteExecution(ctx, "absent") }, storage.ErrNotFound},
		{"RotateRefresh", func() error { return store.RotateRefresh(ctx, "absent", hash, later) }, storage.ErrNotFound},
		{"RotateRefreshAt", func() error { return clocked.RotateRefreshAt(ctx, "absent", hash, later, time.Now()) }, storage.ErrNotFound},
		{"RevokeSession", func() error { return store.RevokeSession(ctx, "absent", time.Now()) }, storage.ErrNotFound},
		{"an empty ID", func() error { return store.DeleteAgent(ctx, "") }, storage.ErrInvalidArgument},
		{"a nil agent", func() error { return store.UpsertAgent(ctx, nil) }, storage.ErrInvalidArgument},
		{"DeleteSessionsByUser matching nothing", func() error { return store.DeleteSessionsByUser(ctx, "nobody") }, nil},
		{"DeleteVerifierByCredential matching nothing", func() error { return store.DeleteVerifierByCredential(ctx, "none") }, nil},
		{"DeleteRolloutsBySpec matching nothing", func() error { return store.DeleteRolloutsBySpec(ctx, "none") }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := leader.profile.raft.LastIndex()
			if err := tc.write(); !errors.Is(err, tc.want) {
				t.Fatalf("write = %v, want %v", err, tc.want)
			}
			if commands := commandsAfter(t, leader, before); commands != 0 {
				t.Fatalf("the write appended %d command entries to the log, want none", commands)
			}
		})
	}

	// A write that is valid is proposed, once; a repeat of it is refused before.
	session := clockSession(t)
	before := leader.profile.raft.LastIndex()
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if commands := commandsAfter(t, leader, before); commands != 1 {
		t.Fatalf("CreateSession appended %d command entries to the log, want 1", commands)
	}
	before = leader.profile.raft.LastIndex()
	if err := store.CreateSession(ctx, session); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("CreateSession of a session that exists = %v, want %v", err, storage.ErrAlreadyExists)
	}
	if commands := commandsAfter(t, leader, before); commands != 0 {
		t.Fatalf("a repeated CreateSession appended %d command entries to the log, want none", commands)
	}

	// No replica applied a command that failed, so none logged one.
	for _, n := range c.nodes {
		waitFor(t, 5*time.Second, n.id+" applies the log", func() bool {
			return n.profile.raft.AppliedIndex() >= leader.profile.raft.LastIndex()
		})
		if failed := failedCommands(t, logs[n.id]); len(failed) != 0 {
			t.Fatalf("%s logged failed commands: %v", n.id, failed)
		}
	}
}

// The single-operation writes that carry a clock, RotateRefreshAt and
// RevokeSession, keep it through the overlay: the modification time the caller
// chose is the one every replica stores, the epoch included, which only a time
// that is present can tell from none.
func TestSingleWritesKeepTheSessionClock(t *testing.T) {
	c := newLoopbackCluster(t, "n1", "n2", "n3")
	leader := c.leader(t, 10*time.Second)
	store := leader.profile.Store()
	clocked, ok := store.(storage.ClockedSessionRotation)
	if !ok {
		t.Fatalf("%T has no explicit session rotation timestamps", store)
	}
	ctx := context.Background()
	session := clockSession(t)
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}

	// converged waits until every replica holds the session as the leader does,
	// and returns it.
	converged := func(step string) (updatedAt, revokedAt time.Time, hash []byte) {
		t.Helper()
		want, err := leader.profile.inner.GetSession(ctx, session.ID())
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range c.nodes {
			waitFor(t, 5*time.Second, n.id+" applies "+step, func() bool {
				got, err := n.profile.inner.GetSession(ctx, session.ID())
				return err == nil && proto.Equal(wire.SessionToProto(got), wire.SessionToProto(want))
			})
		}
		return want.UpdatedAt(), want.RevokedAt(), want.RefreshHash()
	}

	epoch := time.Unix(0, 0).UTC()
	rotatedAt := time.Date(2026, 1, 1, 12, 30, 0, 123, time.UTC)
	revokedAt := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	for _, step := range []struct {
		name string
		do   func() error
		want time.Time // the modification time the step leaves
	}{
		{"a rotation at a time", func() error {
			return clocked.RotateRefreshAt(ctx, session.ID(), []byte("rotated-hash"), session.ExpiresAt().Add(time.Hour), rotatedAt)
		}, rotatedAt},
		{"a rotation at the epoch", func() error {
			return clocked.RotateRefreshAt(ctx, session.ID(), []byte("epoch-hash"), session.ExpiresAt().Add(2*time.Hour), epoch)
		}, epoch},
		{"a revocation", func() error { return store.RevokeSession(ctx, session.ID(), revokedAt) }, revokedAt},
	} {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		updatedAt, gotRevokedAt, hash := converged(step.name)
		if !updatedAt.Equal(step.want) {
			t.Fatalf("%s left UpdatedAt %s on every replica, want %s", step.name, updatedAt.Format(time.RFC3339Nano), step.want.Format(time.RFC3339Nano))
		}
		if step.name == "a revocation" && (!gotRevokedAt.Equal(revokedAt) || string(hash) != "epoch-hash") {
			t.Fatalf("%s left RevokedAt %s and hash %q", step.name, gotRevokedAt.Format(time.RFC3339Nano), hash)
		}
	}
}
