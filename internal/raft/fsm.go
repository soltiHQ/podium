package raft

import (
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Compile-time check.
var _ hraft.FSM = (*FSM)(nil)

// FSM applies replicated commands to the underlying storage.
//
// Every Apply runs inside one inner WithTx: either all ops commit, or none.
// The FSM runs on every replica on every committed log entry, keeping state
// identical across the cluster.
type FSM struct {
	store storage.Storage
}

// NewFSM builds an FSM wrapping store. store must be the plain inmemory one
// (NOT a Raft-backed wrapper) to avoid recursive Apply.
func NewFSM(store storage.Storage) *FSM { return &FSM{store: store} }

// Apply decodes the log entry and executes every op inside a single WithTx.
func (f *FSM) Apply(l *hraft.Log) any {
	cmd, err := DecodeCommand(l.Data)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return f.store.WithTx(ctx, func(tx storage.Storage) error {
		for i, op := range cmd.Ops {
			if err := applyOp(ctx, tx, op); err != nil {
				return fmt.Errorf("op[%d] %d: %w", i, op.Code, err)
			}
		}
		return nil
	})
}

func applyOp(ctx context.Context, tx storage.Storage, op Op) error {
	switch op.Code {
	case OpAgentUpsert:
		a, err := wire.AgentFromDTO(op.AgentUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertAgent(ctx, a)
	case OpAgentDelete:
		return tx.DeleteAgent(ctx, op.ID)

	case OpUserUpsert:
		u, err := wire.UserFromDTO(op.UserUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertUser(ctx, u)
	case OpUserDelete:
		return tx.DeleteUser(ctx, op.ID)

	case OpRoleUpsert:
		r, err := wire.RoleFromDTO(op.RoleUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertRole(ctx, r)
	case OpRoleDelete:
		return tx.DeleteRole(ctx, op.ID)

	case OpCredentialUpsert:
		c, err := wire.CredentialFromDTO(op.CredentialUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertCredential(ctx, c)
	case OpCredentialDelete:
		return tx.DeleteCredential(ctx, op.ID)

	case OpVerifierUpsert:
		v, err := wire.VerifierFromDTO(op.VerifierUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertVerifier(ctx, v)
	case OpVerifierDelete:
		return tx.DeleteVerifier(ctx, op.ID)
	case OpVerifierDeleteByCredential:
		return tx.DeleteVerifierByCredential(ctx, op.ID)

	case OpSessionCreate:
		s, err := wire.SessionFromDTO(op.SessionCreate)
		if err != nil {
			return err
		}
		return tx.CreateSession(ctx, s)
	case OpSessionDelete:
		return tx.DeleteSession(ctx, op.ID)
	case OpSessionDeleteByUser:
		return tx.DeleteSessionsByUser(ctx, op.ID)
	case OpSessionRotateRefresh:
		return tx.RotateRefresh(ctx, op.ID, op.RefreshHash, time.Unix(0, op.ExpiresAtNs))
	case OpSessionRevoke:
		return tx.RevokeSession(ctx, op.ID, time.Unix(0, op.RevokedAtNs))

	case OpSpecUpsert:
		ts, err := wire.SpecFromDTO(op.SpecUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertSpec(ctx, ts)
	case OpSpecDelete:
		return tx.DeleteSpec(ctx, op.ID)

	case OpRolloutUpsert:
		r, err := wire.RolloutFromDTO(op.RolloutUpsert)
		if err != nil {
			return err
		}
		return tx.UpsertRollout(ctx, r)
	case OpRolloutDelete:
		return tx.DeleteRollout(ctx, op.ID)
	case OpRolloutDeleteBySpec:
		return tx.DeleteRolloutsBySpec(ctx, op.ID)

	default:
		return fmt.Errorf("unknown op code %d", op.Code)
	}
}

// Snapshot returns a point-in-time snapshot for log compaction.
type fsmSnapshot struct {
	data []byte
}

func (f *FSM) Snapshot() (hraft.FSMSnapshot, error) {
	// A full state dump would be written here; Phase 1 uses the empty
	// snapshot so the log is self-sufficient for recovery. Acceptable for
	// low-write CP workloads. Followers that miss too many entries will
	// re-bootstrap via fresh log replay.
	return &fsmSnapshot{}, nil
}

func (f *FSM) Restore(r io.ReadCloser) error {
	defer r.Close()
	// Nothing to restore in the empty-snapshot scheme; log replay rebuilds
	// state.
	var discard []byte
	dec := gob.NewDecoder(r)
	_ = dec.Decode(&discard)
	return nil
}

func (s *fsmSnapshot) Persist(sink hraft.SnapshotSink) error { return sink.Close() }
func (s *fsmSnapshot) Release()                              {}
