package backend

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

type AgentsListResult struct {
	Items      []*model.Agent
	NextCursor string
}

type Agents struct {
	store storage.AgentStore
}

func NewAgents(store storage.AgentStore) *Agents {
	return &Agents{store: store}
}

// List returns agents list (filter/pagination later).
func (a *Agents) List(ctx context.Context, limit int, cursor string) (*AgentsListResult, error) {
	if limit <= 0 {
		limit = 100
	}

	res, err := a.store.ListAgents(
		ctx,
		nil, // filter
		storage.ListOptions{
			Limit:  limit,
			Cursor: cursor,
		},
	)
	if err != nil {
		return nil, err
	}

	return &AgentsListResult{
		Items:      res.Items,
		NextCursor: res.NextCursor,
	}, nil
}
