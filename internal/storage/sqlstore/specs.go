package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

var (
	specKind   = kind[*model.Spec]{"podium_specs", snapshot.UnmarshalSpecJSON}
	upsertSpec = upsertSQL("podium_specs", "created_at", "body")
)

// UpsertSpec creates or replaces a spec. A spec whose config the snapshot message
// cannot represent is refused with storage.ErrInvalidArgument.
func (w *writer) UpsertSpec(ctx context.Context, s *model.Spec) error {
	if err := storage.ValidateEntity(s); err != nil {
		return err
	}
	body, err := snapshot.MarshalSpecJSON(s)
	if err != nil {
		return err
	}
	id, err := indexKey("spec ID", s.ID())
	if err != nil {
		return err
	}
	created, err := columnTime("creation time", s.CreatedAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, upsertSpec, id, created, body)
	return err
}

// GetSpec returns the spec with the ID.
func (r *reader) GetSpec(ctx context.Context, id string) (*model.Spec, error) {
	return byID(ctx, r, specKind, id)
}

// ListSpecs returns a page of the specs the filter matches.
func (r *reader) ListSpecs(ctx context.Context, filter storage.SpecFilter, opts storage.ListOptions) (*storage.SpecListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, err := specQuery(filter)
	if err != nil {
		return nil, err
	}
	return listPage(ctx, r, q, opts)
}

// DeleteSpec deletes the spec with the ID.
func (w *writer) DeleteSpec(ctx context.Context, id string) error {
	return w.deleteByID(ctx, specKind.table, id)
}

// UpsertSpec creates or replaces a spec, in a write transaction of its own.
func (s *Store) UpsertSpec(ctx context.Context, spec *model.Spec) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertSpec(ctx, spec) })
}

// ListSpecs returns a page of the specs the filter matches, from the replica for
// a read-only context.
func (s *Store) ListSpecs(ctx context.Context, filter storage.SpecFilter, opts storage.ListOptions) (*storage.SpecListResult, error) {
	return routed(ctx, s, func(r *reader) (*storage.SpecListResult, error) { return r.ListSpecs(ctx, filter, opts) })
}

// DeleteSpec deletes the spec with the ID, in a write transaction of its own.
func (s *Store) DeleteSpec(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteSpec(ctx, id) })
}
