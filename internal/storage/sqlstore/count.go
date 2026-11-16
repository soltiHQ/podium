package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/enum"
)

// countByStatus counts the rows of table by their status column, in one GROUP BY
// statement: an empty map, not nil, for no row, and no entry for a status no row
// has.
func countByStatus[S ~uint8](ctx context.Context, r *reader, table string) (map[S]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := r.q.QueryContext(ctx, "SELECT status, count(*) FROM "+table+" GROUP BY status")
	if err != nil {
		return nil, r.fail(ctx, err)
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[S]int)
	for rows.Next() {
		var status int16
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, r.fail(ctx, err)
		}
		counts[S(status)] = int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, r.fail(ctx, err)
	}
	return counts, nil
}

// CountAgents counts the agents by status.
func (r *reader) CountAgents(ctx context.Context) (map[enum.AgentStatus]int, error) {
	return countByStatus[enum.AgentStatus](ctx, r, agentKind.table)
}

// CountRollouts counts the rollouts by sync status.
func (r *reader) CountRollouts(ctx context.Context) (map[enum.SyncStatus]int, error) {
	return countByStatus[enum.SyncStatus](ctx, r, rolloutKind.table)
}

// CountAgents counts the agents by status, on the replica for a read-only
// context.
func (s *Store) CountAgents(ctx context.Context) (map[enum.AgentStatus]int, error) {
	return routed(ctx, s, func(r *reader) (map[enum.AgentStatus]int, error) { return r.CountAgents(ctx) })
}

// CountRollouts counts the rollouts by sync status, as CountAgents counts agents.
func (s *Store) CountRollouts(ctx context.Context) (map[enum.SyncStatus]int, error) {
	return routed(ctx, s, func(r *reader) (map[enum.SyncStatus]int, error) { return r.CountRollouts(ctx) })
}
