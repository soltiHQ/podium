package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// A rollout's row holds its spec, its agent and its sync status beside the body,
// for the criteria, the bulk delete and the counts.

var (
	rolloutKind   = kind[*model.Rollout]{"podium_rollouts", snapshot.UnmarshalRolloutJSON}
	upsertRollout = upsertSQL("podium_rollouts", "created_at", "spec_id", "agent_id", "status", "body")
)

// UpsertRollout creates or replaces a rollout.
func (w *writer) UpsertRollout(ctx context.Context, r *model.Rollout) error {
	if err := storage.ValidateEntity(r); err != nil {
		return err
	}
	body, err := snapshot.MarshalRolloutJSON(r)
	if err != nil {
		return err
	}
	id, err := indexKey("rollout ID", r.ID())
	if err != nil {
		return err
	}
	spec, err := indexKey("rollout's spec ID", r.SpecID())
	if err != nil {
		return err
	}
	agent, err := indexKey("rollout's agent ID", r.AgentID())
	if err != nil {
		return err
	}
	created, err := columnTime("creation time", r.CreatedAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, upsertRollout, id, created, spec, agent, int16(r.Status()), body)
	return err
}

// GetRollout returns the rollout with the ID.
func (r *reader) GetRollout(ctx context.Context, id string) (*model.Rollout, error) {
	return byID(ctx, r, rolloutKind, id)
}

// ListRollouts returns a page of the rollouts the filter matches.
func (r *reader) ListRollouts(ctx context.Context, filter storage.RolloutFilter, opts storage.ListOptions) (*storage.RolloutListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, err := rolloutQuery(filter)
	if err != nil {
		return nil, err
	}
	return listPage(ctx, r, q, opts)
}

// DeleteRollout deletes the rollout with the ID.
func (w *writer) DeleteRollout(ctx context.Context, id string) error {
	return w.deleteByID(ctx, rolloutKind.table, id)
}

// DeleteRolloutsBySpec deletes every rollout of the spec; a spec with none is no
// error.
func (w *writer) DeleteRolloutsBySpec(ctx context.Context, specID string) error {
	if specID == "" {
		return storage.ErrInvalidArgument
	}
	return w.deleteWhere(ctx, rolloutKind.table, "spec_id = ?", bin(specID))
}

// UpsertRollout creates or replaces a rollout, in a write transaction of its own.
func (s *Store) UpsertRollout(ctx context.Context, r *model.Rollout) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertRollout(ctx, r) })
}

// ListRollouts returns a page of the rollouts the filter matches, from the
// replica for a read-only context.
func (s *Store) ListRollouts(ctx context.Context, filter storage.RolloutFilter, opts storage.ListOptions) (*storage.RolloutListResult, error) {
	return routed(ctx, s, func(r *reader) (*storage.RolloutListResult, error) { return r.ListRollouts(ctx, filter, opts) })
}

// DeleteRollout deletes the rollout with the ID, in a write transaction of its
// own.
func (s *Store) DeleteRollout(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteRollout(ctx, id) })
}

// DeleteRolloutsBySpec deletes every rollout of the spec, in a write transaction
// of its own.
func (s *Store) DeleteRolloutsBySpec(ctx context.Context, specID string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteRolloutsBySpec(ctx, specID) })
}
