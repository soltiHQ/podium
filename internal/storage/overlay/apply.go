package overlay

import (
	"context"
	"fmt"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// ApplyOps applies the store operations of a SOLTI-CMD command to dst, in order:
// each one is converted back from its wire form and passed to the dst method that
// makes it. It stops at the first operation dst refuses and returns that error,
// wrapped with the index and type of the operation. dst should be the handle of a
// transaction, which the error then rolls back, so that a command applies whole or
// not at all. An event operation, or one ApplyOps does not know, is an error
// wrapping wire.ErrUnknownOp.
func ApplyOps(ctx context.Context, dst storage.Storage, ops []*raftv1.Op) error {
	for i, op := range ops {
		if err := applyOp(ctx, dst, op); err != nil {
			return fmt.Errorf("op[%d] %T: %w", i, op.GetOp(), err)
		}
	}
	return nil
}

// applyOp dispatches one store operation to dst. The variant of op drives the
// type switch; each case converts the proto sub-message via wire.*FromProto and
// calls the corresponding dst method.
func applyOp(ctx context.Context, dst storage.Storage, op *raftv1.Op) error {
	switch v := op.GetOp().(type) {
	case *raftv1.Op_AgentUpsert:
		a, err := wire.AgentFromProto(v.AgentUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertAgent(ctx, a)
	case *raftv1.Op_AgentDelete:
		return dst.DeleteAgent(ctx, v.AgentDelete)

	case *raftv1.Op_AgentCredentialUpsert:
		c, err := wire.AgentCredentialFromProto(v.AgentCredentialUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertAgentCredential(ctx, c)
	case *raftv1.Op_AgentCredentialDelete:
		return dst.DeleteAgentCredential(ctx, v.AgentCredentialDelete)

	case *raftv1.Op_UserUpsert:
		u, err := wire.UserFromProto(v.UserUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertUser(ctx, u)
	case *raftv1.Op_UserDelete:
		return dst.DeleteUser(ctx, v.UserDelete)

	case *raftv1.Op_RoleUpsert:
		r, err := wire.RoleFromProto(v.RoleUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertRole(ctx, r)
	case *raftv1.Op_RoleDelete:
		return dst.DeleteRole(ctx, v.RoleDelete)

	case *raftv1.Op_CredentialUpsert:
		c, err := wire.CredentialFromProto(v.CredentialUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertCredential(ctx, c)
	case *raftv1.Op_CredentialDelete:
		return dst.DeleteCredential(ctx, v.CredentialDelete)

	case *raftv1.Op_VerifierUpsert:
		ver, err := wire.VerifierFromProto(v.VerifierUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertVerifier(ctx, ver)
	case *raftv1.Op_VerifierDelete:
		return dst.DeleteVerifier(ctx, v.VerifierDelete)
	case *raftv1.Op_VerifierDeleteByCred:
		return dst.DeleteVerifierByCredential(ctx, v.VerifierDeleteByCred)

	case *raftv1.Op_SessionCreate:
		s, err := wire.SessionFromProto(v.SessionCreate)
		if err != nil {
			return err
		}
		return dst.CreateSession(ctx, s)
	case *raftv1.Op_SessionDelete:
		return dst.DeleteSession(ctx, v.SessionDelete)
	case *raftv1.Op_SessionDeleteByUser:
		return dst.DeleteSessionsByUser(ctx, v.SessionDeleteByUser)
	case *raftv1.Op_SessionRotateRefresh:
		m := v.SessionRotateRefresh
		if m == nil {
			return storage.ErrInvalidArgument
		}
		clocked, ok := dst.(storage.ClockedSessionRotation)
		if !ok {
			return fmt.Errorf("%w: explicit session rotation timestamps are unsupported", storage.ErrInternal)
		}
		hash, expiresAt, updatedAt := rotation(m)
		return clocked.RotateRefreshAt(ctx, m.GetId(), hash, expiresAt, updatedAt)
	case *raftv1.Op_SessionRevoke:
		m := v.SessionRevoke
		return dst.RevokeSession(ctx, m.GetId(), revocation(m))

	case *raftv1.Op_SpecUpsert:
		ts, err := wire.SpecFromProto(v.SpecUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertSpec(ctx, ts)
	case *raftv1.Op_SpecDelete:
		return dst.DeleteSpec(ctx, v.SpecDelete)

	case *raftv1.Op_RolloutUpsert:
		r, err := wire.RolloutFromProto(v.RolloutUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertRollout(ctx, r)
	case *raftv1.Op_RolloutDelete:
		return dst.DeleteRollout(ctx, v.RolloutDelete)
	case *raftv1.Op_RolloutDeleteBySpec:
		return dst.DeleteRolloutsBySpec(ctx, v.RolloutDeleteBySpec)

	case *raftv1.Op_ExecutionUpsert:
		e, err := wire.ExecutionFromProto(v.ExecutionUpsert)
		if err != nil {
			return err
		}
		return dst.UpsertExecution(ctx, e)
	case *raftv1.Op_ExecutionDelete:
		return dst.DeleteExecution(ctx, v.ExecutionDelete)
	default:
		return fmt.Errorf("%w: %T", wire.ErrUnknownOp, op.GetOp())
	}
}

// RotateRefreshOp is the operation that rotates the refresh token of a session.
// It records the presence of updatedAt apart from its Unix nanosecond value: a nil
// UpdatedAtNs is the historical operation, which preserves UpdatedAt, and a
// pointer to zero is the epoch.
func RotateRefreshOp(sessionID string, newHash []byte, newExpiresAt, updatedAt time.Time) *raftv1.Op {
	var updatedAtNs *int64
	if !updatedAt.IsZero() {
		ns := updatedAt.UnixNano()
		updatedAtNs = &ns
	}
	return &raftv1.Op{Op: &raftv1.Op_SessionRotateRefresh{
		SessionRotateRefresh: &raftv1.SessionRotateRefreshMsg{
			Id:          sessionID,
			RefreshHash: append([]byte(nil), newHash...),
			ExpiresAtNs: newExpiresAt.UnixNano(),
			UpdatedAtNs: updatedAtNs,
		},
	}}
}

// rotation returns the refresh hash, expiry and modification time a rotation
// applies. Historical commands omitted the modification time: it is then the zero
// time, with which the clocked storage hook preserves the stored modification time
// rather than inventing one.
func rotation(m *raftv1.SessionRotateRefreshMsg) (hash []byte, expiresAt, updatedAt time.Time) {
	if m.UpdatedAtNs != nil {
		updatedAt = time.Unix(0, m.GetUpdatedAtNs())
	}
	return m.GetRefreshHash(), time.Unix(0, m.GetExpiresAtNs()), updatedAt
}

// revocation returns the time a revocation revokes its session at.
func revocation(m *raftv1.SessionRevokeMsg) time.Time {
	return time.Unix(0, m.GetRevokedAtNs())
}
