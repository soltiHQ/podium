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
	shutdownErr := a.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		logger.Error().Err(shutdownErr).Dur("timeout", shutdownTimeout).Msg("shutdown failed")
	}

	if code := exitCode(runErr, shutdownErr); code != 0 {
		logger.Error().Err(runErr).Msg("server exited with error")
		os.Exit(code)
	}
	logger.Info().Msg("server stopped")
}

// Signal cancellation is a clean shutdown; failed cleanup or runner errors are
// visible to a supervisor through a nonzero exit status.
func exitCode(runErr, shutdownErr error) int {
	if shutdownErr != nil {
		return 1
	}
	if runErr != nil && runErr != context.Canceled {
		return 1
	}
	return 0
}
