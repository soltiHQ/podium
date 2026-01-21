package backend

import (
	"context"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/logctx"
	"github.com/soltiHQ/control-plane/internal/storage"

	"github.com/rs/zerolog"
)

// Discovery is a handler for the discovery service.
func Discovery(ctx context.Context, logger zerolog.Logger, store storage.Storage, agent *domain.AgentModel) error {
	logger = logctx.From(ctx, logger)

	if err := store.UpsertAgent(ctx, agent); err != nil {
		logger.Err(err).
			Str("agent_id", agent.ID()).
			Msg("failed to upsert agent")
		return err
	}
	logger.Debug().
		Str("agent_id", agent.ID()).
		Msg("agent synced")
	return nil
}
