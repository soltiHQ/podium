package storage

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
)

// WorkloadSchemaListResult is a page of the workload schema catalog.
type WorkloadSchemaListResult = ListResult[*model.WorkloadSchema]

// WorkloadSchemaStore keeps the catalog of workload schemas, keyed by
// fingerprint. A fingerprint names its schema bytes, so an entry never
// changes: UpsertWorkloadSchema is a no-op for a fingerprint already recorded,
// and the first agent and time stay those of its first record.
type WorkloadSchemaStore interface {
	UpsertWorkloadSchema(context.Context, *model.WorkloadSchema) error
	// GetWorkloadSchema returns ErrNotFound for a fingerprint not in the catalog.
	GetWorkloadSchema(ctx context.Context, fingerprint string) (*model.WorkloadSchema, error)
	ListWorkloadSchemas(ctx context.Context, opts ListOptions) (*WorkloadSchemaListResult, error)
}
