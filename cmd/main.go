package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/app"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/service/agent"
)

// shutdownTimeout caps how long [App.Shutdown] may take. If any cleanup
// step (Raft fsync, hub drain, pool close) hangs past this, the process
// exits dirty rather than holding the runtime forever.
const shutdownTimeout = 30 * time.Second

func main() {
	// The subcommand needs no configuration: it runs before the config loads.
	if len(os.Args) > 1 && os.Args[1] == "agent-token" {
		os.Exit(agentToken(os.Args[2:], os.Stdout, os.Stderr))
	}

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

// agentToken runs "podium agent-token <agent-id>": it prints the token that
// agent ID enrolls with while the enrollment secret is set, and a newline, and
// returns the exit code. It reads the secret from config.EnrollSecretEnv only
// and applies the checks of the server: the secret has at least
// agent.MinEnrollSecretBytes bytes and the ID passes model.ValidateAgentID. A
// failure prints one line to stderr, never the secret, and nothing to stdout.
func agentToken(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: podium agent-token <agent-id>")
		return 2
	}
	id := args[0]
	if err := model.ValidateAgentID(id); err != nil {
		_, _ = fmt.Fprintf(stderr, "podium: %v\n", err)
		return 1
	}
	secret := os.Getenv(config.EnrollSecretEnv)
	switch {
	case secret == "":
		_, _ = fmt.Fprintf(stderr, "podium: %s is not set\n", config.EnrollSecretEnv)
		return 1
	case len(secret) < agent.MinEnrollSecretBytes:
		_, _ = fmt.Fprintf(stderr, "podium: %s must have at least %d bytes\n", config.EnrollSecretEnv, agent.MinEnrollSecretBytes)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, agent.EnrollToken(secret, id)); err != nil {
		_, _ = fmt.Fprintf(stderr, "podium: write the token: %v\n", err)
		return 1
	}
	return 0
}
