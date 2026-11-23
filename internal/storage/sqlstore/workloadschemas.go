package sqlstore

import (
	"context"
	"fmt"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// A workload schema's row holds its fingerprint and its first-seen time beside
// the body. The first record of a fingerprint is immutable.

var (
	workloadSchemaKind   = kind[*model.WorkloadSchema]{"podium_workload_schemas", snapshot.UnmarshalWorkloadSchemaJSON}
	insertWorkloadSchema = insertIfAbsent("podium_workload_schemas", "created_at", "body")
)

// UpsertWorkloadSchema records a schema under its fingerprint when none is
// recorded: a replay changes nothing and is no error.
func (w *writer) UpsertWorkloadSchema(ctx context.Context, s *model.WorkloadSchema) error {
	if s == nil {
		return storage.ErrInvalidArgument
	}
	next, err := model.NewWorkloadSchema(s.ID(), s.APIVersion(), s.Kind(), s.Schema(), s.FirstAgentID(), s.FirstSeenAt())
	if err != nil {
		return fmt.Errorf("%w: sqlstore: the workload schema: %v", storage.ErrInvalidArgument, err)
	}
	body, err := snapshot.MarshalWorkloadSchemaJSON(next)
	if err != nil {
		return err
	}
	id, err := indexKey("workload schema fingerprint", next.ID())
	if err != nil {
		return err
	}
	created, err := columnTime("first-seen time", next.FirstSeenAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, insertWorkloadSchema, id, created, body)
	return err
}

// GetWorkloadSchema returns the schema with the fingerprint.
func (r *reader) GetWorkloadSchema(ctx context.Context, fingerprint string) (*model.WorkloadSchema, error) {
	return byID(ctx, r, workloadSchemaKind, fingerprint)
}

// ListWorkloadSchemas returns a page of the catalog.
func (r *reader) ListWorkloadSchemas(ctx context.Context, opts storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return listPage(ctx, r, pageQuery[*model.WorkloadSchema]{kind: workloadSchemaKind, where: &where{}}, opts)
}

// UpsertWorkloadSchema records a schema, in a write transaction of its own.
func (s *Store) UpsertWorkloadSchema(ctx context.Context, w *model.WorkloadSchema) error {
	return s.write(ctx, func(h *writer) error { return h.UpsertWorkloadSchema(ctx, w) })
}

// ListWorkloadSchemas returns a page of the catalog, from the replica for a
// read-only context.
func (s *Store) ListWorkloadSchemas(ctx context.Context, opts storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
	return routed(ctx, s, func(r *reader) (*storage.WorkloadSchemaListResult, error) { return r.ListWorkloadSchemas(ctx, opts) })
}
