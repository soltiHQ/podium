package main

import (
	"context"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"

	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/apiserver"
	"github.com/soltiHQ/control-plane/internal/transport/edgeserver"
	"github.com/soltiHQ/control-plane/internal/transport/webserver"
)

func main() {
	// global logger
	zerolog.TimeFieldFormat = time.RFC3339
	logger := log.With().Str("app", "lighthouse").Logger()

	// storage layer
	store := inmemory.New()

	// compose servers
	edge := edgeserver.NewEdgeServer(
		edgeserver.NewConfig(
			edgeserver.WithHTTPAddr(":8081"),
			edgeserver.WithGRPCAddr(":50051"),
			edgeserver.WithLogLevel(zerolog.DebugLevel),
		),
		logger,
		store,
	)

	api := apiserver.NewApiServer(
		apiserver.NewConfig(
			apiserver.WithHTTPAddr(":8082"),
			apiserver.WithLogLevel(zerolog.DebugLevel),
		),
		logger,
		store,
	)

	web := webserver.NewWebServer(
		webserver.NewConfig(
			webserver.WithHTTPAddr(":8080"),
			webserver.WithLogLevel(zerolog.DebugLevel),
		),
		logger,
	)

	// master context (cancel on signals)
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// graceful orchestrator
	g, ctx := errgroup.WithContext(rootCtx)

	// edge server goroutine
	g.Go(func() error {
		logger.Info().Msg("starting edge server")
		return edge.Run(ctx)
	})

	// api server goroutine
	g.Go(func() error {
		logger.Info().Msg("starting api server")
		return api.Run(ctx)
	})

	// web server goroutine
	g.Go(func() error {
		logger.Info().Msg("starting web server")
		return web.Run(ctx)
	})

	// wait for first error OR signal
	if err := g.Wait(); err != nil {
		logger.Error().Err(err).Msg("lighthouse terminated with error")
	} else {
		logger.Info().Msg("lighthouse stopped cleanly")
	}
}
