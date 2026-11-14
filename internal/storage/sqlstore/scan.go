package sqlstore

import (
	"context"
	"database/sql"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// snapshot runs read on one consistent state of the database: a read-only
// REPEATABLE READ transaction of its own on r's pool, or the transaction r
// already reads, which is one state in itself.
func (r *reader) snapshot(ctx context.Context, read func(r *reader) error) error {
	if r.db == nil {
		return read(r)
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return r.fail(ctx, err)
	}
	// The snapshot writes nothing: ending it is a rollback.
	defer func() { _ = tx.Rollback() }()
	return read(&reader{d: r.d, q: tx})
}

// queryAll returns every row of k that w holds for, decoded, in the listing
// order and filtered by match, nil for none. It reads in one statement: a
// caller that needs a consistent state around it takes a snapshot.
func queryAll[T any](ctx context.Context, r *reader, k kind[T], w *where, match func(T) bool) ([]T, error) {
	bodies, err := r.bodies(ctx, "SELECT body FROM "+k.table+w.sql()+" ORDER BY created_at DESC, id ASC", 0, w.args...)
	if err != nil {
		return nil, err
	}
	var items []T
	for _, body := range bodies {
		item, err := k.decode(body)
		if err != nil {
			return nil, err
		}
		if match == nil || match(item) {
			items = append(items, item)
		}
	}
	return items, nil
}

// scan returns every row q holds for, in one snapshot: what draining the
// listing returns, as a slice the caller owns, nil for none.
func scan[T any](ctx context.Context, r *reader, q pageQuery[T]) ([]T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var items []T
	err := r.snapshot(ctx, func(r *reader) error {
		var err error
		items, err = queryAll(ctx, r, q.kind, q.where, q.match)
		return err
	})
	return items, err
}

// ScanAgents returns every agent the filter matches.
func (r *reader) ScanAgents(ctx context.Context, filter storage.AgentFilter) ([]*model.Agent, error) {
	q, err := agentQuery(filter)
	if err != nil {
		return nil, err
	}
	return scan(ctx, r, q)
}

// ScanRollouts returns every rollout the filter matches.
func (r *reader) ScanRollouts(ctx context.Context, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	q, err := rolloutQuery(filter)
	if err != nil {
		return nil, err
	}
	return scan(ctx, r, q)
}

// ScanSpecs returns every spec the filter matches.
func (r *reader) ScanSpecs(ctx context.Context, filter storage.SpecFilter) ([]*model.Spec, error) {
	q, err := specQuery(filter)
	if err != nil {
		return nil, err
	}
	return scan(ctx, r, q)
}

// ScanExecutions returns every execution record the filter matches.
func (r *reader) ScanExecutions(ctx context.Context, filter storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	q, err := executionQuery(filter)
	if err != nil {
		return nil, err
	}
	return scan(ctx, r, q)
}

// ScanAgents returns every agent the filter matches, in one snapshot, from the
// replica for a read-only context.
func (s *Store) ScanAgents(ctx context.Context, filter storage.AgentFilter) ([]*model.Agent, error) {
	return routed(ctx, s, func(r *reader) ([]*model.Agent, error) { return r.ScanAgents(ctx, filter) })
}

// ScanRollouts returns every rollout the filter matches, as ScanAgents does agents.
func (s *Store) ScanRollouts(ctx context.Context, filter storage.RolloutFilter) ([]*model.Rollout, error) {
	return routed(ctx, s, func(r *reader) ([]*model.Rollout, error) { return r.ScanRollouts(ctx, filter) })
}

// ScanSpecs returns every spec the filter matches, as ScanAgents does agents.
func (s *Store) ScanSpecs(ctx context.Context, filter storage.SpecFilter) ([]*model.Spec, error) {
	return routed(ctx, s, func(r *reader) ([]*model.Spec, error) { return r.ScanSpecs(ctx, filter) })
}

// ScanExecutions returns every execution record the filter matches, as ScanAgents
// does agents.
func (s *Store) ScanExecutions(ctx context.Context, filter storage.ExecutionFilter) ([]*model.ExecutionRecord, error) {
	return routed(ctx, s, func(r *reader) ([]*model.ExecutionRecord, error) { return r.ScanExecutions(ctx, filter) })
}

// routed runs read on a reader of the database that serves a listing, a scan or
// a count of s outside a transaction: the replica, for a context marked
// storage.MarkReadOnly, when the store has one, and the primary otherwise.
func routed[T any](ctx context.Context, s *Store, read func(r *reader) (T, error)) (T, error) {
	var out T
	err := s.listing(ctx, func(db *sql.DB) error {
		var err error
		out, err = read(&reader{d: s.dialect, q: db, db: db})
		return err
	})
	return out, err
}
