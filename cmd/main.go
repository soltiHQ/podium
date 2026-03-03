package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	genv1 "github.com/soltiHQ/control-plane/api/gen/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/wire"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
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
	var (
		logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
		store  = inmemory.New()
	)

	var (
		jwtSecret = "dev-secret-change-me-in-production"
		authModel = wire.NewAuth(
			store,
			jwtSecret,
			1*time.Minute,
			7*24*time.Hour,
			1*time.Minute,
			2,
		)
	)

	var (
		authSVC       = access.New(authModel, store, logger)
		roleSVC       = role.New(store)
		userSVC       = user.New(store, logger)
		sessionSVC    = session.New(store)
		credentialSVC = credential.New(store, logger)
		agentSVC      = agent.New(store)
		specSVC       = spec.New(store)
	)

	// Bootstrap roles + admin user
	if err := bootstrap.Run(context.Background(), logger, roleSVC, userSVC, credentialSVC); err != nil {
		logger.Fatal().Err(err).Msg("failed to bootstrap")
	}

	proxyPool := proxy.NewPool()
	defer proxyPool.Close()

	lifecycleRunner, err := lifecycle.New(lifecycle.Config{}, logger, store)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create lifecycle runner")
	}

	syncRunner, err := syncrunner.New(syncrunner.Config{}, logger, store, proxyPool)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create sync runner")
	}

	var (
		jsonResp = responder.NewJSON()
		htmlResp = responder.NewHTML()
	)
	var (
		uiHandler     = handler.NewUI(logger, authSVC)
		apiHandler    = handler.NewAPI(logger, userSVC, authSVC, sessionSVC, credentialSVC, agentSVC, specSVC, proxyPool)
		staticHandler = handler.NewStatic(logger)
	)
	authMW := middleware.Auth(authModel.Verifier, authModel.Session)
	permMW := route.PermMW(func(p kind.Permission) route.BaseMW {
		return middleware.RequirePermission(p)
	})

	mux := http.NewServeMux()
	staticHandler.Routes(mux)
	uiHandler.Routes(mux,
		authMW,
		permMW,
	)
	var (
		ridMW = middleware.RequestID()
		logMW = middleware.Logger(logger)
	)
	apiHandler.Routes(mux,
		authMW,
		permMW,
		ridMW,
		logMW,
	)

	var mainHandler http.Handler = mux
	mainHandler = middleware.Negotiate(jsonResp, htmlResp)(mainHandler)
	mainHandler = middleware.Recovery(logger)(mainHandler)

	httpRunner, err := httpserver.New(
		httpserver.Config{Name: "http", Addr: ":8080"},
		logger,
		mainHandler,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http server")
	}

	// ---------------------------------------------------------------
	// HTTP Discovery :8082
	// ---------------------------------------------------------------
	httpDiscovery := handler.NewHTTPDiscovery(logger, agentSVC)

	discMux := http.NewServeMux()
	discMux.HandleFunc("/api/v1/discovery/sync", httpDiscovery.Sync)

	var discHandler http.Handler = discMux
	discHandler = middleware.Recovery(logger)(discHandler)

	httpDiscoveryRunner, err := httpserver.New(
		httpserver.Config{Name: "http-discovery", Addr: ":8082"},
		logger,
		discHandler,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http discovery server")
	}

	// ---------------------------------------------------------------
	// gRPC Discovery :50051
	// ---------------------------------------------------------------
	grpcDiscovery := handler.NewGRPCDiscovery(logger, agentSVC)

	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			interceptor.UnaryRecovery(logger),
		),
	)
	genv1.RegisterDiscoverServiceServer(grpcSrv, grpcDiscovery)

	grpcRunner, err := grpcserver.New(
		grpcserver.Config{Name: "grpc-discovery", Addr: ":50051"},
		logger,
		grpcSrv,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create grpc server")
	}

	// ---------------------------------------------------------------
	// Server (5 runners)
	// ---------------------------------------------------------------
	srv, err := server.New(server.Config{}, logger, httpRunner, httpDiscoveryRunner, grpcRunner, lifecycleRunner, syncRunner)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create server")
	}

	// Run
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err = srv.Run(ctx); err != nil {
		logger.Error().Err(err).Msg("server exited")
		os.Exit(1)
	}
	logger.Info().Msg("server stopped")
}
