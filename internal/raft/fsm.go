package raft

import (
	"context"
	"fmt"
	"io"
	"time"

	hraft "github.com/hashicorp/raft"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
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
type FSM struct {
	store storage.Storage
	hub   *event.Hub
}

// NewFSM builds an FSM wrapping store and hub. store must be the plain
// inmemory one (NOT a Raft-backed wrapper) to avoid recursive Apply; hub
// must be the local *event.Hub whose ApplyLocal* methods we call directly.
func NewFSM(store storage.Storage, hub *event.Hub) *FSM {
	return &FSM{store: store, hub: hub}
}

// Apply decodes the proto Command, runs store ops under one WithTx, and
// fires event ops on the local hub afterwards.
func (f *FSM) Apply(l *hraft.Log) any {
	cmd, err := decodeCommand(l.Data)
	if err != nil {
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
			for i, op := range storeOps {
				if err := applyStoreOp(ctx, tx, op); err != nil {
					return fmt.Errorf("op[%d] %T: %w", i, op.GetOp(), err)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	for _, op := range eventOps {
		applyEventOp(f.hub, op)
	}
	return nil
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

// applyStoreOp dispatches a store mutation to tx. The variant of op
// drives the type-switch; each case converts the proto sub-message via
// wire.*FromProto and calls the corresponding tx method.
func applyStoreOp(ctx context.Context, tx storage.Storage, op *raftv1.Op) error {
	switch v := op.GetOp().(type) {
	case *raftv1.Op_AgentUpsert:
		a, err := wire.AgentFromProto(v.AgentUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertAgent(ctx, a)
	case *raftv1.Op_AgentDelete:
		return tx.DeleteAgent(ctx, v.AgentDelete)

	case *raftv1.Op_AgentCredentialUpsert:
		c, err := wire.AgentCredentialFromProto(v.AgentCredentialUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertAgentCredential(ctx, c)
	case *raftv1.Op_AgentCredentialDelete:
		return tx.DeleteAgentCredential(ctx, v.AgentCredentialDelete)

	case *raftv1.Op_UserUpsert:
		u, err := wire.UserFromProto(v.UserUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertUser(ctx, u)
	case *raftv1.Op_UserDelete:
		return tx.DeleteUser(ctx, v.UserDelete)

	case *raftv1.Op_RoleUpsert:
		r, err := wire.RoleFromProto(v.RoleUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertRole(ctx, r)
	case *raftv1.Op_RoleDelete:
		return tx.DeleteRole(ctx, v.RoleDelete)

	case *raftv1.Op_CredentialUpsert:
		c, err := wire.CredentialFromProto(v.CredentialUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertCredential(ctx, c)
	case *raftv1.Op_CredentialDelete:
		return tx.DeleteCredential(ctx, v.CredentialDelete)

	case *raftv1.Op_VerifierUpsert:
		ver, err := wire.VerifierFromProto(v.VerifierUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertVerifier(ctx, ver)
	case *raftv1.Op_VerifierDelete:
		return tx.DeleteVerifier(ctx, v.VerifierDelete)
	case *raftv1.Op_VerifierDeleteByCred:
		return tx.DeleteVerifierByCredential(ctx, v.VerifierDeleteByCred)

	case *raftv1.Op_SessionCreate:
		s, err := wire.SessionFromProto(v.SessionCreate)
		if err != nil {
			return err
		}
		return tx.CreateSession(ctx, s)
	case *raftv1.Op_SessionDelete:
		return tx.DeleteSession(ctx, v.SessionDelete)
	case *raftv1.Op_SessionDeleteByUser:
		return tx.DeleteSessionsByUser(ctx, v.SessionDeleteByUser)
	case *raftv1.Op_SessionRotateRefresh:
		m := v.SessionRotateRefresh
		if m == nil {
			return storage.ErrInvalidArgument
		}
		clocked, ok := tx.(storage.ClockedSessionRotation)
		if !ok {
			return fmt.Errorf("%w: explicit session rotation timestamps are unsupported", storage.ErrInternal)
		}
		var updatedAt time.Time
		if m.UpdatedAtNs != nil {
			updatedAt = time.Unix(0, m.GetUpdatedAtNs())
		}
		// Historical commands omitted this field. The clocked storage hook
		// preserves the existing modification time rather than inventing one.
		return clocked.RotateRefreshAt(ctx, m.GetId(), m.GetRefreshHash(), time.Unix(0, m.GetExpiresAtNs()), updatedAt)
	case *raftv1.Op_SessionRevoke:
		m := v.SessionRevoke
		return tx.RevokeSession(ctx, m.GetId(), time.Unix(0, m.GetRevokedAtNs()))

	case *raftv1.Op_SpecUpsert:
		ts, err := wire.SpecFromProto(v.SpecUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertSpec(ctx, ts)
	case *raftv1.Op_SpecDelete:
		return tx.DeleteSpec(ctx, v.SpecDelete)

	case *raftv1.Op_RolloutUpsert:
		r, err := wire.RolloutFromProto(v.RolloutUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertRollout(ctx, r)
	case *raftv1.Op_RolloutDelete:
		return tx.DeleteRollout(ctx, v.RolloutDelete)
	case *raftv1.Op_RolloutDeleteBySpec:
		return tx.DeleteRolloutsBySpec(ctx, v.RolloutDeleteBySpec)

	case *raftv1.Op_ExecutionUpsert:
		e, err := wire.ExecutionFromProto(v.ExecutionUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertExecution(ctx, e)
	case *raftv1.Op_ExecutionDelete:
		return tx.DeleteExecution(ctx, v.ExecutionDelete)
	default:
		return fmt.Errorf("%w: %T", wire.ErrUnknownOp, op.GetOp())
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
	defer r.Close()

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
