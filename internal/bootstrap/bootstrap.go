package bootstrap

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/storage"

	"github.com/rs/zerolog"
)

type Step interface {
	// Name is used for logs.
	Name() string
	// Run applies the step idempotently.
	Run(ctx context.Context, logger zerolog.Logger, store storage.Storage) error
}

// Run bootstrap data initialization.
func Run(ctx context.Context, logger zerolog.Logger, store storage.Storage) error {
	logger = logger.With().Str("type", "bootstrap").Logger()

	steps := []Step{
		EnsureAdminRoleStep{},
		EnsureAdminUserStep{},
	}
	for _, s := range steps {
		if err := s.Run(ctx, logger, store); err != nil {
			logger.Error().Str("step", s.Name()).Err(err).Msg("bootstrap: failed")
			return err
		}
		logger.Debug().Str("step", s.Name()).Msg("bootstrap: ok")
	}
	return nil
}
