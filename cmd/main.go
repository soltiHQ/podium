package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/handlers"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
)

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Responders.
	jsonResp := response.NewJSON()

	// Handlers.
	demo := handlers.NewDemo(jsonResp)

	// Router.
	mux := http.NewServeMux()
	demo.Routes(mux)

	// Middleware chain.
	// CORS → RequestID → Logger → Recovery → Handler
	cors := middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
	})

	var handler http.Handler = mux
	handler = middleware.Recovery(logger)(handler)
	handler = middleware.Logger(logger)(handler)
	handler = middleware.RequestID()(handler)
	handler = cors(handler)

	// HTTP server runner.
	httpRunner, err := httpserver.New(
		httpserver.Config{
			Name: "http",
			Addr: ":8080",
		},
		logger,
		handler,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http server")
	}

	// Server orchestrator.
	srv, err := server.New(
		server.Config{},
		logger,
		httpRunner,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create server")
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info().Msg("starting server on :8080")

	if err := srv.Run(ctx); err != nil {
		logger.Error().Err(err).Msg("server exited")
		os.Exit(1)
	}

	logger.Info().Msg("server stopped")
}
