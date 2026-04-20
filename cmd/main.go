package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	genv1 "github.com/soltiHQ/control-plane/api/gen/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/wire"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
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
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	raftpkg "github.com/soltiHQ/control-plane/internal/raft"
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

	store, leadership, raftShutdown, err := buildCluster(cfg.Cluster, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to build cluster")
	}
	if raftShutdown != nil {
		defer raftShutdown()
	}

	var (
		authModel = wire.NewAuth(store, cfg.Auth)
		svc       = initServices(store, authModel, logger)
	)

	// Default users/roles/credentials seeding must run through the store.
	// In raft mode that means only the leader actually persists — followers
	// would get "not the leader" on Upsert. Solution: wait for any leader,
	// then only the leader seeds. If this replica is a follower, skip —
	// leader's writes will replicate here automatically.
	if waitForLeader(context.Background(), leadership, 30*time.Second) && leadership.AmLeader() {
		if err = bootstrap.Run(context.Background(), logger, svc.role, svc.user, svc.credential); err != nil {
			logger.Fatal().Err(err).Msg("failed to bootstrap")
		}
	} else {
		logger.Info().Msg("bootstrap skipped: this replica is a follower; leader seeds shared state")
	}

	proxyPool := proxy.NewPool()
	defer proxyPool.Close()

	eventHub := event.NewHub(logger)
	defer eventHub.Close()

	htmx.Configure(cfg.Triggers)

	lifecycleRunner, err := lifecycle.New(cfg.Lifecycle, logger, store, eventHub, leadership)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create lifecycle runner")
	}

	syncRunner, err := syncrunner.New(cfg.Sync, logger, store, proxyPool, eventHub, leadership)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create sync runner")
	}

	mainHandler := buildMainHandler(cfg, logger, svc, authModel, proxyPool, eventHub, leadership)
	httpRunner, err := httpserver.New(cfg.HTTP, logger, mainHandler)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http server")
	}

	discoveryHandler := buildDiscoveryHandler(logger, svc.agent, eventHub, leadership, addrPort(cfg.HTTPDiscovery.Addr))
	httpDiscoveryRunner, err := httpserver.New(cfg.HTTPDiscovery, logger, discoveryHandler)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http discovery server")
	}

	grpcSrv := buildGRPCServer(logger, svc.agent, eventHub, leadership)
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

func initServices(store storage.Storage, authModel *wire.Auth, logger zerolog.Logger) services {
	return services{
		access:     access.New(authModel, store, logger),
		credential: credential.New(store, logger),
		session:    session.New(store, logger),
		agent:      agent.New(store, logger),
		spec:       spec.New(store, logger),
		role:       role.New(store, logger),
		user:       user.New(store, logger),
	}
}

// buildCluster constructs the storage.Storage + cluster.Leadership pair
// declared by cfg. Returns (store, leadership, shutdownFunc, err).
func buildCluster(cfg cluster.Config, logger zerolog.Logger) (storage.Storage, cluster.Leadership, func(), error) {
	inner := inmemory.New()
	switch cfg.Backend {
	case "", cluster.BackendStandalone:
		return inner, standalone.NewLeadership(), nil, nil
	case cluster.BackendRaft:
		disco, err := buildDiscovery(cfg.Discovery)
		if err != nil {
			return nil, nil, nil, err
		}
		p, err := raftpkg.New(raftpkg.Config{
			NodeID:             cfg.Raft.NodeID,
			BindAddr:           cfg.Raft.BindAddr,
			AdvertiseAddr:      cfg.Raft.AdvertiseAddr,
			DataDir:            cfg.Raft.DataDir,
			ElectionTimeout:    msToDuration(cfg.Raft.ElectionTimeoutMs),
			HeartbeatTimeout:   msToDuration(cfg.Raft.HeartbeatTimeoutMs),
		}, inner, disco, logger)
		if err != nil {
			return nil, nil, nil, err
		}
		return p.Store(), p.Leadership(), func() { _ = p.Shutdown() }, nil
	default:
		return nil, nil, nil, fmt.Errorf("cluster: unknown backend %q", cfg.Backend)
	}
}

func buildDiscovery(cfg cluster.DiscoveryConfig) (cluster.Discovery, error) {
	switch cfg.Driver {
	case "", "static":
		peers := make([]cluster.Peer, 0, len(cfg.Peers))
		for _, addr := range cfg.Peers {
			peers = append(peers, cluster.Peer{ID: addr, Address: addr})
		}
		return discovery.NewStatic(peers), nil
	case "dns":
		return discovery.NewDNS(cfg.Hostname, cfg.Port)
	default:
		return nil, fmt.Errorf("discovery: unknown driver %q", cfg.Driver)
	}
}

func msToDuration(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// waitForLeader polls until any leader is elected or timeout elapses.
// Returns true if a leader exists at return (which might be us or a peer).
// Used on startup so subsequent leader-only work (bootstrap, singleton
// runners) runs against a stable cluster, not a half-formed one.
func waitForLeader(ctx context.Context, l cluster.Leadership, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		// AmLeader() || CurrentLeader() != "" means someone is leader.
		if l.AmLeader() || l.CurrentLeader() != "" {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return l.AmLeader() || l.CurrentLeader() != ""
}

func buildMainHandler(cfg config.Config, logger zerolog.Logger, svc services, authModel *wire.Auth, proxyPool *proxy.Pool, eventHub *event.Hub, leadership cluster.Leadership) http.Handler {
	var (
		apiHandler    = handler.NewAPI(logger, svc.user, svc.access, svc.session, svc.credential, svc.agent, svc.spec, proxyPool, eventHub)
		authMW        = middleware.Auth(authModel.Verifier, authModel.Session)
		uiHandler     = handler.NewUI(logger, svc.access, svc.spec, eventHub)
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
	h = middleware.Leader(leadership, middleware.LeaderOptions{
		ForwardPort: addrPort(cfg.HTTP.Addr),
	})(h)
	h = middleware.CORS(cfg.CORS)(h)
	h = middleware.Recovery(logger)(h)
	return h
}

func buildDiscoveryHandler(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, leadership cluster.Leadership, httpPort int) http.Handler {
	var (
		httpDiscovery = handler.NewHTTPDiscovery(logger, agentSVC, eventHub)
		mux           = http.NewServeMux()
	)
	mux.HandleFunc("/api/v1/discovery/sync", httpDiscovery.Sync)

	var h http.Handler = mux
	h = middleware.Leader(leadership, middleware.LeaderOptions{ForwardPort: httpPort})(h)
	h = middleware.Recovery(logger)(h)
	h = middleware.Logger(logger)(h)
	h = middleware.RequestID()(h)
	return h
}

// addrPort extracts the port number from a listen address like ":8080" or
// "0.0.0.0:8080". Returns 0 if the address is malformed.
func addrPort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return 0
	}
	return n
}

func buildGRPCServer(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, leadership cluster.Leadership) *grpc.Server {
	// Write methods (must run on leader). Sync is the only agent-facing
	// mutation; everything else is pure read.
	writeMethods := map[string]struct{}{
		genv1.DiscoverService_Sync_FullMethodName: {},
	}
	isWrite := func(full string) bool { _, ok := writeMethods[full]; return ok }

	var (
		srv = grpc.NewServer(
			grpc.ChainUnaryInterceptor(
				interceptor.UnaryRecovery(logger),
				interceptor.UnaryRequestID(),
				interceptor.UnaryLogger(logger),
				interceptor.UnaryLeader(leadership, interceptor.LeaderOptions{IsWrite: isWrite}),
			),
		)
		grpcDiscovery = handler.NewGRPCDiscovery(logger, agentSVC, eventHub)
	)
	genv1.RegisterDiscoverServiceServer(srv, grpcDiscovery)
	return srv
}
