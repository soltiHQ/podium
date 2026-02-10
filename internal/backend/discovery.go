package backend

import (
	"context"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Discovery handles agent discovery sync (upsert) use-case.
type Discovery struct {
	store storage.Storage
}

func NewDiscovery(store storage.Storage) *Discovery {
	return &Discovery{store: store}
}

func (x *Discovery) Sync(ctx context.Context, logger zerolog.Logger, agent *model.Agent) error {
	if agent == nil {
		return storage.ErrInvalidArgument
	}

	if err := x.store.UpsertAgent(ctx, agent); err != nil {
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
