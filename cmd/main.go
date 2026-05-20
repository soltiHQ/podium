package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/app"
	"github.com/soltiHQ/control-plane/internal/config"
)

// shutdownTimeout caps how long [App.Shutdown] may take. If any cleanup
// step (Raft fsync, hub drain, pool close) hangs past this, the process
// exits dirty rather than holding the runtime forever.
const shutdownTimeout = 30 * time.Second

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load config")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to build app")
	}

	runErr := a.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		logger.Error().Err(err).Dur("timeout", shutdownTimeout).Msg("shutdown deadline exceeded; exiting dirty")
	}

	if runErr != nil {
		logger.Error().Err(runErr).Msg("server exited")
		os.Exit(1)
	}
	logger.Info().Msg("server stopped")
}
