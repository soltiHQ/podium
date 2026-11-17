package overlay

import (
	"context"
	"errors"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// UpsertWorkloadSchema keeps the first record of a fingerprint: writing one the
// transaction sees records nothing, as the state machine would change nothing.
func (t *Tx) UpsertWorkloadSchema(ctx context.Context, w *model.WorkloadSchema) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return storage.ErrInvalidArgument
	}
	switch _, err := t.GetWorkloadSchema(ctx, w.ID()); {
	case err == nil:
		return nil
	case !errors.Is(err, storage.ErrNotFound):
		return err
	}
	msg := wire.WorkloadSchemaToProto(w)
	row, err := wire.WorkloadSchemaFromProto(msg)
	if err = decoded(row, err); err != nil {
		return err
	}
	t.workloadSchemas.put(row)
	t.record(&raftv1.Op{Op: &raftv1.Op_WorkloadSchemaUpsert{WorkloadSchemaUpsert: msg}})
	return nil
}

func (t *Tx) GetWorkloadSchema(ctx context.Context, fingerprint string) (*model.WorkloadSchema, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t.workloadSchemas.get(ctx, fingerprint, t.base.GetWorkloadSchema)
}

func (t *Tx) ListWorkloadSchemas(ctx context.Context, opts storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return list(opts, func() ([]*model.WorkloadSchema, error) {
		inBase, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.WorkloadSchemaListResult, error) {
			return t.base.ListWorkloadSchemas(ctx, o)
		})
		if err != nil {
			return nil, err
		}
		return merged(inBase, &t.workloadSchemas, nil), nil
	})
}
