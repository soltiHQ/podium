package sqlstore

import (
	"slices"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// The filters are what FilterFactory builds from a backend-agnostic set of
// criteria. Each splits its criteria as the spec says: SQL applies the exact
// ones (equalities, sets, the staleness bound) and the keyset order; Go applies
// a Query, as storage.TextQuery, over the rows SQL returns, so that a query
// matches here exactly as in memory, whatever the engine's collation and case
// folding.

// filters is the store's FilterFactory, and the transaction handle's.
type filters struct{}

// AgentFilter is the agent filter this store builds: a Query in Go, the
// staleness bound in SQL.
type AgentFilter struct {
	query storage.TextQuery
	// staleBefore bounds the staleness deadline: a deadline before it, or none,
	// matches. Zero: no bound.
	staleBefore time.Time
}

// BuildAgentFilter builds the filter of c.
func (filters) BuildAgentFilter(c storage.AgentQueryCriteria) storage.AgentFilter {
	return &AgentFilter{query: storage.NewTextQuery(c.Query), staleBefore: c.StaleAtBefore}
}

// agentQuery resolves the filter of a listing or a scan to its query: nil
// matches every agent, one this store did not build, or a nil *AgentFilter, is
// refused with storage.ErrInvalidArgument.
func agentQuery(filter storage.AgentFilter) (pageQuery[*model.Agent], error) {
	f := &AgentFilter{}
	switch typed := filter.(type) {
	case nil:
	case *AgentFilter:
		if typed == nil {
			return pageQuery[*model.Agent]{}, storage.ErrInvalidArgument
		}
		f = typed
	default:
		return pageQuery[*model.Agent]{}, storage.ErrInvalidArgument
	}

	w := &where{}
	if !f.staleBefore.IsZero() {
		// The deadline is 0 when never set, which always matches.
		switch ns, pos := timeBound(f.staleBefore); pos {
		case inRange:
			w.add("(stale_at = 0 OR stale_at < ?)", ns)
		case belowRange:
			// Every deadline set lies after the bound.
			w.add("stale_at = 0")
		case aboveRange:
			// Every deadline set lies before it: no condition.
		}
	}
	q := pageQuery[*model.Agent]{kind: agentKind, where: w}
	if !f.query.Empty() {
		q.match = func(a *model.Agent) bool { return f.query.Matches(a.ID(), a.Name(), a.Endpoint()) }
	}
	return q, nil
}

// SpecFilter is the spec filter this store builds: a Query, in Go.
type SpecFilter struct {
	query storage.TextQuery
}

// BuildSpecFilter builds the filter of c.
func (filters) BuildSpecFilter(c storage.SpecQueryCriteria) storage.SpecFilter {
	return &SpecFilter{query: storage.NewTextQuery(c.Query)}
}

// specQuery resolves the filter of a listing or a scan, as agentQuery does.
func specQuery(filter storage.SpecFilter) (pageQuery[*model.Spec], error) {
	f := &SpecFilter{}
	switch typed := filter.(type) {
	case nil:
	case *SpecFilter:
		if typed == nil {
			return pageQuery[*model.Spec]{}, storage.ErrInvalidArgument
		}
		f = typed
	default:
		return pageQuery[*model.Spec]{}, storage.ErrInvalidArgument
	}
	q := pageQuery[*model.Spec]{kind: specKind, where: &where{}}
	if !f.query.Empty() {
		q.match = func(s *model.Spec) bool { return f.query.Matches(s.Name(), s.Slot()) }
	}
	return q, nil
}

// RolloutFilter is the rollout filter this store builds: every criterion is
// exact, in SQL.
type RolloutFilter struct {
	specID, agentID string
	statuses        []enum.SyncStatus
}

// BuildRolloutFilter builds the filter of c.
func (filters) BuildRolloutFilter(c storage.RolloutQueryCriteria) storage.RolloutFilter {
	return &RolloutFilter{specID: c.SpecID, agentID: c.AgentID, statuses: slices.Clone(c.Statuses)}
}

// rolloutQuery resolves the filter of a listing or a scan, as agentQuery does.
func rolloutQuery(filter storage.RolloutFilter) (pageQuery[*model.Rollout], error) {
	f := &RolloutFilter{}
	switch typed := filter.(type) {
	case nil:
	case *RolloutFilter:
		if typed == nil {
			return pageQuery[*model.Rollout]{}, storage.ErrInvalidArgument
		}
		f = typed
	default:
		return pageQuery[*model.Rollout]{}, storage.ErrInvalidArgument
	}
	w := &where{}
	if f.specID != "" {
		w.add("spec_id = ?", bin(f.specID))
	}
	if f.agentID != "" {
		w.add("agent_id = ?", bin(f.agentID))
	}
	if len(f.statuses) > 0 {
		args := make([]any, len(f.statuses))
		for i, s := range f.statuses {
			args[i] = int16(s)
		}
		w.add("status IN ("+marks(len(args))+")", args...)
	}
	return pageQuery[*model.Rollout]{kind: rolloutKind, where: w}, nil
}

// ExecutionFilter is the execution filter this store builds: every criterion is
// exact, in SQL.
type ExecutionFilter struct {
	criteria storage.ExecutionQueryCriteria
}

// BuildExecutionFilter builds the filter of c.
func (filters) BuildExecutionFilter(c storage.ExecutionQueryCriteria) storage.ExecutionFilter {
	c.Statuses = slices.Clone(c.Statuses)
	return &ExecutionFilter{criteria: c}
}

// executionQuery resolves the filter of a listing or a scan, as agentQuery does.
func executionQuery(filter storage.ExecutionFilter) (pageQuery[*model.ExecutionRecord], error) {
	f := &ExecutionFilter{}
	switch typed := filter.(type) {
	case nil:
	case *ExecutionFilter:
		if typed == nil {
			return pageQuery[*model.ExecutionRecord]{}, storage.ErrInvalidArgument
		}
		f = typed
	default:
		return pageQuery[*model.ExecutionRecord]{}, storage.ErrInvalidArgument
	}
	c := f.criteria
	w := &where{}
	for _, exact := range [...]struct{ column, value string }{
		{"agent_id", c.AgentID},
		{"spec_id", c.SpecID},
		{"task_name", c.TaskName},
		{"task_uid", c.TaskUID},
	} {
		if exact.value != "" {
			w.add(exact.column+" = ?", bin(exact.value))
		}
	}
	if len(c.Statuses) > 0 {
		// A stored record's status is terminal, so another matches no row; it is
		// left out of the statement, which a text column could not take whatever
		// the status held (a NUL, bytes that are not UTF-8).
		var args []any
		for _, s := range c.Statuses {
			if model.IsTerminalExecutionStatus(s) {
				args = append(args, s)
			}
		}
		if len(args) == 0 {
			w.add("FALSE")
		} else {
			w.add("status IN ("+marks(len(args))+")", args...)
		}
	}
	return pageQuery[*model.ExecutionRecord]{kind: executionKind, where: w}, nil
}
