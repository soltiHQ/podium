package raft

import (
	"context"
	"fmt"
	"io"
	"strings"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// Compile-time check.
var _ hraft.FSM = (*FSM)(nil)

// FSM applies replicated commands to the underlying storage and hub.
//
// Store mutations run inside one WithTx per command (all-or-nothing).
// Event ops (Notify/Record/DeleteIssues) are applied to the hub outside
// the transaction — they are append-only and have no rollback semantics.
// The FSM runs on every replica on every committed log entry.
//
// A command that cannot be decoded or fails to apply changes nothing on the
// replica and is dropped. Only the proposing leader sees the error, as the
// response of Apply, so every replica logs it (see logFailure).
type FSM struct {
	store  storage.Storage
	hub    *event.Hub
	logger zerolog.Logger
}

// NewFSM builds an FSM wrapping store and hub. store must be the plain
// inmemory one (NOT a Raft-backed wrapper) to avoid recursive Apply; hub
// must be the local *event.Hub whose ApplyLocal* methods we call directly.
// logger gets a line for each command that fails.
func NewFSM(store storage.Storage, hub *event.Hub, logger zerolog.Logger) *FSM {
	return &FSM{store: store, hub: hub, logger: logger}
}

// Apply decodes the proto Command, runs store ops under one WithTx, and
// fires event ops on the local hub afterwards.
func (f *FSM) Apply(l *hraft.Log) any {
	cmd, err := overlay.DecodeCommand(l.Data)
	if err != nil {
		f.logFailure(l.Index, nil)
		return err
	}
	ctx := context.Background()

	// Split ops: store ops go through WithTx, event ops are applied
	// directly on the hub after the tx commits successfully.
	var (
		storeOps []*raftv1.Op
		eventOps []*raftv1.Op
	)
	for _, op := range cmd.GetOps() {
		if isEventOp(op) {
			eventOps = append(eventOps, op)
		} else {
			storeOps = append(storeOps, op)
		}
	}

	if len(storeOps) > 0 {
		err := f.store.WithTx(ctx, func(tx storage.Storage) error {
			return overlay.ApplyOps(ctx, tx, storeOps)
		})
		if err != nil {
			f.logFailure(l.Index, cmd)
			return err
		}
	}

	for _, op := range eventOps {
		applyEventOp(f.hub, op)
	}
	return nil
}

// undecodable is the kind logged for a command that cannot be decoded.
const undecodable = "undecodable"

// logFailure logs a command that failed on this replica, at ERROR: the index of
// its log entry, its kind and the number of its operations, and nothing more.
// Neither the error nor the command is logged, as either can hold the values of
// the state (IDs, names, hashes). The kind of a decoded command is the kinds of
// its operations, named as in the protocol (agent_upsert), in order, each once;
// a command that does not decode (cmd is nil) has the kind "undecodable" and no
// count.
//
// Store checks every write against the state before it proposes it, so a command
// that fails here is a corrupted log entry or a version skew. Every replica
// applies a log entry the same way, so each one logs the same entry, unless a
// version skew lets some of them apply what others cannot. The line is then the
// only sign of the divergence.
func (f *FSM) logFailure(index uint64, cmd *raftv1.Command) {
	if cmd == nil {
		f.logger.Error().Uint64("index", index).Str("kind", undecodable).
			Msg("raft: committed command is not decodable and was dropped on this replica (corrupted entry or version skew)")
		return
	}
	f.logger.Error().Uint64("index", index).Str("kind", commandKind(cmd)).Int("ops", len(cmd.GetOps())).
		Msg("raft: committed command failed to apply and was dropped on this replica, which stays as it was (corrupted entry or version skew)")
}

// commandKind names the kinds of the operations of cmd, in order, each once,
// joined by commas: agent_upsert,agent_delete.
func commandKind(cmd *raftv1.Command) string {
	var (
		kinds []string
		seen  = make(map[string]bool)
	)
	for _, op := range cmd.GetOps() {
		if kind := opKind(op); !seen[kind] {
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	return strings.Join(kinds, ",")
}

// opKind is the name in the protocol of the variant op carries.
func opKind(op *raftv1.Op) string {
	m := op.ProtoReflect()
	if field := m.WhichOneof(m.Descriptor().Oneofs().ByName("op")); field != nil {
		return string(field.Name())
	}
	return "unknown"
}

// isEventOp reports whether op carries an event-hub mutation rather than
// a store mutation.
func isEventOp(op *raftv1.Op) bool {
	switch op.GetOp().(type) {
	case *raftv1.Op_EventNotify, *raftv1.Op_EventRecord, *raftv1.Op_EventDeleteIssues:
		return true
	}
	return false
}

// applyEventOp dispatches a hub mutation to the local replica.
func applyEventOp(hub *event.Hub, op *raftv1.Op) {
	if hub == nil {
		return
	}
	switch v := op.GetOp().(type) {
	case *raftv1.Op_EventNotify:
		hub.ApplyLocalNotify(v.EventNotify)
	case *raftv1.Op_EventRecord:
		msg := v.EventRecord
		hub.ApplyLocalRecord(msg.GetKind(), payloadFromProto(msg.GetPayload()))
	case *raftv1.Op_EventDeleteIssues:
		msg := v.EventDeleteIssues
		hub.ApplyLocalDeleteIssues(msg.GetKind(), msg.GetId())
	}
}

// Snapshot captures the current store state for log compaction.
//
// The returned hraft.FSMSnapshot holds a decoupled copy of every entity
// (shallow-copied maps under each store's read lock, values are clones).
// Apply may run concurrently with the subsequent Persist call — the
// snapshot is not observed by later mutations.
//
// Currently only *inmemory.Store is supported; passing any other backend
// at construction time results in a descriptive error here.
func (f *FSM) Snapshot() (hraft.FSMSnapshot, error) {
	store, ok := f.store.(*inmemory.Store)
	if !ok {
		return nil, fmt.Errorf("raft snapshot: requires *inmemory.Store, got %T", f.store)
	}
	return &fsmSnapshot{content: store.SnapshotForRaft()}, nil
}

// Restore replaces the FSM state with the contents of r.
//
// The proto blob is fully decoded into memory before the live store is
// swapped, so a truncated or corrupted stream leaves the store untouched.
func (f *FSM) Restore(r io.ReadCloser) error {
	defer func() { _ = r.Close() }()

	store, ok := f.store.(*inmemory.Store)
	if !ok {
		return fmt.Errorf("raft restore: requires *inmemory.Store, got %T", f.store)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("raft restore: read: %w", err)
	}
	content, err := snapshot.Unmarshal(data)
	if err != nil {
		return fmt.Errorf("raft restore: %w", err)
	}

	store.RestoreFromSnapshot(content)
	return nil
}
