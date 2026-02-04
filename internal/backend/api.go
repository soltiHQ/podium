package backend

import (
	"context"

	"github.com/soltiHQ/control-plane/auth"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transportctx"

	"github.com/rs/zerolog"
)

// AgentList returns a list of all agents.
func AgentList(ctx context.Context, logger zerolog.Logger, store storage.Storage) ([]*domain.AgentModel, error) {
	id, ok := transportctx.Identity(ctx)
	if !ok || !id.HasPermission(string(domain.PermAgentsGet)) {
		return nil, auth.ErrUnauthorized
	}

	res, err := store.ListAgents(ctx, nil, storage.ListOptions{Limit: storage.DefaultListLimit})
	if err != nil {
		return nil, err
	}
	return res.Items, nil
}
