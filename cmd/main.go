package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/uikit/trigger"
	"google.golang.org/grpc"

	genv1 "github.com/soltiHQ/control-plane/api/gen/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/wire"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/handler"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/grpcserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/lifecycle"
	syncrunner "github.com/soltiHQ/control-plane/internal/server/runner/sync"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/role"
	"github.com/soltiHQ/control-plane/internal/service/session"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/grpc/interceptor"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
)

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
	cfg, err := config.Load()
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load config")
	}

	var (
		store     = inmemory.New()
		authModel = wire.NewAuth(store, cfg.Auth)

		authSVC       = access.New(authModel, store, logger)
		credentialSVC = credential.New(store, logger)
		userSVC       = user.New(store, logger)
		sessionSVC    = session.New(store)
		agentSVC      = agent.New(store)
		specSVC       = spec.New(store)
		roleSVC       = role.New(store)
	)
	trigger.Configure(cfg.Triggers)

	if err = bootstrap.Run(context.Background(), logger, roleSVC, userSVC, credentialSVC); err != nil {
		logger.Fatal().Err(err).Msg("failed to bootstrap")
	}

	proxyPool := proxy.NewPool()
	defer proxyPool.Close()

	lifecycleRunner, err := lifecycle.New(cfg.Lifecycle, logger, store)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create lifecycle runner")
	}

	syncRunner, err := syncrunner.New(cfg.Sync, logger, store, proxyPool)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create sync runner")
	}

	var (
		apiHandler    = handler.NewAPI(logger, userSVC, authSVC, sessionSVC, credentialSVC, agentSVC, specSVC, proxyPool)
		uiHandler     = handler.NewUI(logger, authSVC)
		staticHandler = handler.NewStatic(logger)

		authMW = middleware.Auth(authModel.Verifier, authModel.Session)
		logMW  = middleware.Logger(logger)
		ridMW  = middleware.RequestID()

		permMW = route.PermMW(func(p kind.Permission) route.BaseMW {
			return middleware.RequirePermission(p)
		})
		mux = http.NewServeMux()
	)
	apiHandler.Routes(mux, authMW, permMW, ridMW, logMW)
	uiHandler.Routes(mux, authMW, permMW)
	staticHandler.Routes(mux)

	var mainHandler http.Handler = mux
	mainHandler = middleware.Negotiate(responder.NewJSON(), responder.NewHTML())(mainHandler)
	mainHandler = middleware.CORS(cfg.CORS)(mainHandler)
	mainHandler = middleware.Recovery(logger)(mainHandler)

	httpRunner, err := httpserver.New(cfg.HTTP, logger, mainHandler)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http server")
	}

	var (
		httpDiscovery = handler.NewHTTPDiscovery(logger, agentSVC)
		discMux       = http.NewServeMux()
	)
	discMux.HandleFunc("/api/v1/discovery/sync", httpDiscovery.Sync)

	var discHandler http.Handler = discMux
	discHandler = middleware.Recovery(logger)(discHandler)
	discHandler = middleware.Logger(logger)(discHandler)
	discHandler = middleware.RequestID()(discHandler)

	httpDiscoveryRunner, err := httpserver.New(cfg.HTTPDiscovery, logger, discHandler)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http discovery server")
	}

	var (
		grpcDiscovery = handler.NewGRPCDiscovery(logger, agentSVC)
		grpcSrv       = grpc.NewServer(
			grpc.ChainUnaryInterceptor(
				interceptor.UnaryRecovery(logger),
				interceptor.UnaryRequestID(),
				interceptor.UnaryLogger(logger),
			),
		)
	)
	genv1.RegisterDiscoverServiceServer(grpcSrv, grpcDiscovery)

	grpcRunner, err := grpcserver.New(cfg.GRPC, logger, grpcSrv)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create grpc server")
	}

	srv, err := server.New(cfg.Server, logger, httpRunner, httpDiscoveryRunner, grpcRunner, lifecycleRunner, syncRunner)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create server")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err = srv.Run(ctx); err != nil {
		logger.Error().Err(err).Msg("server exited")
		os.Exit(1)
	}
	logger.Info().Msg("server stopped")
}
