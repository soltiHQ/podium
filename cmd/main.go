package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	genv1 "github.com/soltiHQ/control-plane/api/gen/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/wire"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
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
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

type services struct {
	credential *credential.Service
	session    *session.Service
	access     *access.Service
	agent      *agent.Service
	spec       *spec.Service
	user       *user.Service
	role       *role.Service
}

func main() {
	var (
		logger   = zerolog.New(os.Stdout).With().Timestamp().Logger()
		cfg, err = config.Load()
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load config")
	}

	var (
		store     = inmemory.New()
		authModel = wire.NewAuth(store, cfg.Auth)
		svc       = initServices(store, authModel, logger)
	)
	if err = bootstrap.Run(context.Background(), logger, svc.role, svc.user, svc.credential); err != nil {
		logger.Fatal().Err(err).Msg("failed to bootstrap")
	}

	proxyPool := proxy.NewPool()
	defer proxyPool.Close()

	eventHub := event.NewHub(logger)
	defer eventHub.Close()

	htmx.Configure(cfg.Triggers)

	lifecycleRunner, err := lifecycle.New(cfg.Lifecycle, logger, store, eventHub)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create lifecycle runner")
	}

	syncRunner, err := syncrunner.New(cfg.Sync, logger, store, proxyPool, eventHub)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create sync runner")
	}

	mainHandler := buildMainHandler(cfg, logger, svc, authModel, proxyPool, eventHub)
	httpRunner, err := httpserver.New(cfg.HTTP, logger, mainHandler)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http server")
	}

	discoveryHandler := buildDiscoveryHandler(logger, svc.agent, eventHub)
	httpDiscoveryRunner, err := httpserver.New(cfg.HTTPDiscovery, logger, discoveryHandler)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http discovery server")
	}

	grpcSrv := buildGRPCServer(logger, svc.agent, eventHub)
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

func initServices(store *inmemory.Store, authModel *wire.Auth, logger zerolog.Logger) services {
	return services{
		access:     access.New(authModel, store, logger),
		credential: credential.New(store, logger),
		user:       user.New(store, logger),
		session:    session.New(store),
		agent:      agent.New(store),
		spec:       spec.New(store),
		role:       role.New(store),
	}
}

func buildMainHandler(cfg config.Config, logger zerolog.Logger, svc services, authModel *wire.Auth, proxyPool *proxy.Pool, eventHub *event.Hub) http.Handler {
	var (
		apiHandler    = handler.NewAPI(logger, svc.user, svc.access, svc.session, svc.credential, svc.agent, svc.spec, proxyPool, eventHub)
		authMW        = middleware.Auth(authModel.Verifier, authModel.Session)
		uiHandler     = handler.NewUI(logger, svc.access, eventHub)
		staticHandler = handler.NewStatic(logger)
		logMW         = middleware.Logger(logger)
		ridMW         = middleware.RequestID()
		mux           = http.NewServeMux()

		permMW = route.PermMW(func(p kind.Permission) route.BaseMW {
			return middleware.RequirePermission(p)
		})
	)
	apiHandler.Routes(mux, authMW, permMW, ridMW, logMW)
	uiHandler.Routes(mux, authMW, permMW)
	staticHandler.Routes(mux)

	route.HandleFunc(mux, routepath.ApiEventStream, eventHub.SSEHandler(), ridMW, logMW, authMW)

	var h http.Handler = mux
	h = middleware.Negotiate(responder.NewJSON(), responder.NewHTML())(h)
	h = middleware.CORS(cfg.CORS)(h)
	h = middleware.Recovery(logger)(h)
	return h
}

func buildDiscoveryHandler(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub) http.Handler {
	var (
		httpDiscovery = handler.NewHTTPDiscovery(logger, agentSVC, eventHub)
		mux           = http.NewServeMux()
	)
	mux.HandleFunc("/api/v1/discovery/sync", httpDiscovery.Sync)

	var h http.Handler = mux
	h = middleware.Recovery(logger)(h)
	h = middleware.Logger(logger)(h)
	h = middleware.RequestID()(h)
	return h
}

func buildGRPCServer(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub) *grpc.Server {
	var (
		srv = grpc.NewServer(
			grpc.ChainUnaryInterceptor(
				interceptor.UnaryRecovery(logger),
				interceptor.UnaryRequestID(),
				interceptor.UnaryLogger(logger),
			),
		)
		grpcDiscovery = handler.NewGRPCDiscovery(logger, agentSVC, eventHub)
	)
	genv1.RegisterDiscoverServiceServer(srv, grpcDiscovery)
	return srv
}
