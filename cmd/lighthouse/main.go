package main

import (
	"context"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"

	"github.com/soltiHQ/control-plane/auth"
	"github.com/soltiHQ/control-plane/auth/authenticator"
	authjwt "github.com/soltiHQ/control-plane/auth/jwt"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/apiserver"
	"github.com/soltiHQ/control-plane/internal/transport/edgeserver"
	"github.com/soltiHQ/control-plane/internal/transport/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/webserver"
)

func main() {
	// global logger
	zerolog.TimeFieldFormat = time.RFC3339
	logger := log.With().Str("app", "lighthouse").Logger()

	// master context (cancel on signals)
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// storage layer
	store := inmemory.New()

	// bootstrap (idempotent) TODO:  do not execute if auth_config.go Enabled=false or nil
	if err := bootstrap.Run(rootCtx, logger, store); err != nil {
		logger.Error().Err(err).Msg("bootstrap failed")
		return
	}

	// ---- TEST JWT CONFIG (hardcoded) ----
	jwtCfg := auth.JWTConfig{
		Issuer:   "lighthouse",
		Audience: "lighthouse-api",
		Secret:   []byte("dev-secret-change-me-32bytes-min"), // >= 32 bytes is fine
		TokenTTL: 15 * time.Minute,
	}

	jwtIssuer := authjwt.NewIssuer(jwtCfg.Secret)
	jwtVerifier := authjwt.NewVerifier(jwtCfg.Issuer, jwtCfg.Audience, jwtCfg.Secret)
	authn := authenticator.NewPasswordAuthenticator(store, jwtIssuer, jwtCfg)

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
			apiserver.WithAuthenticator(authn),
			apiserver.WithHTTPMiddlewareConfig(func() middleware.HttpChainConfig {
				h := middleware.DefaultHttpChainConfig()
				h.Auth = &middleware.AuthConfig{
					Enabled:  true,
					Verifier: jwtVerifier,
				}
				return h
			}()),
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

	// graceful orchestrator
	g, ctx := errgroup.WithContext(rootCtx)

	g.Go(func() error {
		logger.Info().Msg("starting edge server")
		return edge.Run(ctx)
	})

	g.Go(func() error {
		logger.Info().Msg("starting api server")
		return api.Run(ctx)
	})

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
