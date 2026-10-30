package overlay

import (
	"context"
	"fmt"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func (t *Tx) UpsertSpec(_ context.Context, ts *model.Spec) error {
	if ts == nil {
		return storage.ErrInvalidArgument
	}
	msg, err := wire.SpecToProto(ts)
	if err != nil {
		return fmt.Errorf("%w: %w", storage.ErrInvalidArgument, err)
	}
	row, err := wire.SpecFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.specs.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_SpecUpsert{SpecUpsert: msg}})
	return nil
}

func (t *Tx) GetSpec(ctx context.Context, id string) (*model.Spec, error) {
	return t.specs.get(ctx, id, t.base.GetSpec)
}

func (t *Tx) ListSpecs(ctx context.Context, filter storage.SpecFilter, opts storage.ListOptions) (*storage.SpecListResult, error) {
	return list(opts, func() ([]*model.Spec, error) { return t.ScanSpecs(ctx, filter) })
}

func (t *Tx) ScanSpecs(ctx context.Context, filter storage.SpecFilter) ([]*model.Spec, error) {
	return scan(&t.specs, filter, func() ([]*model.Spec, error) { return storage.AllSpecs(ctx, t.base, filter) })
}

func (t *Tx) DeleteSpec(ctx context.Context, id string) error {
	if err := t.specs.remove(ctx, id, t.base.GetSpec); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_SpecDelete{SpecDelete: id}})
	return nil
}

func (t *Tx) UpsertRollout(_ context.Context, r *model.Rollout) error {
	if r == nil {
		return storage.ErrInvalidArgument
	}
	msg := wire.RolloutToProto(r)
	row, err := wire.RolloutFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.rollouts.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_RolloutUpsert{RolloutUpsert: msg}})
	return nil
}

func (t *Tx) GetRollout(ctx context.Context, id string) (*model.Rollout, error) {
	return t.rollouts.get(ctx, id, t.base.GetRollout)
}

func (t *Tx) ListRollouts(ctx context.Context, filter storage.RolloutFilter, opts storage.ListOptions) (*storage.RolloutListResult, error) {
	return list(opts, func() ([]*model.Rollout, error) { return t.ScanRollouts(ctx, filter) })
}

func (t *Tx) ScanRollouts(ctx context.Context, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	return scan(&t.rollouts, filter, func() ([]*model.Rollout, error) { return storage.AllRollouts(ctx, t.base, filter) })
}

func (t *Tx) DeleteRollout(ctx context.Context, id string) error {
	if err := t.rollouts.remove(ctx, id, t.base.GetRollout); err != nil {
		return err
	}
	t.record(&raftv1.Op{Op: &raftv1.Op_RolloutDelete{RolloutDelete: id}})
	return nil
}

func (t *Tx) DeleteRolloutsBySpec(ctx context.Context, specID string) error {
	if specID == "" {
		return storage.ErrInvalidArgument
	}
	inBase, err := storage.AllRollouts(ctx, t.base, t.base.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: specID}))
	if err != nil {
		return err
	}
	t.rollouts.removeAll(inBase, func(r *model.Rollout) bool { return r.SpecID() == specID })
	t.record(&raftv1.Op{Op: &raftv1.Op_RolloutDeleteBySpec{RolloutDeleteBySpec: specID}})
	return nil
}
