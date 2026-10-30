package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/handler"
	"github.com/soltiHQ/control-plane/internal/loghub"
	"github.com/soltiHQ/control-plane/internal/proxy"
	raftpkg "github.com/soltiHQ/control-plane/internal/raft"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/service/role"
	"github.com/soltiHQ/control-plane/internal/service/session"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/durable"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
	"github.com/soltiHQ/control-plane/internal/transport/grpc/interceptor"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

// services groups all domain services constructed at startup.
type services struct {
	credential *credential.Service
	history    *history.Service
	session    *session.Service
	access     *access.Service
	agent      *agent.Service
	spec       *spec.Service
	user       *user.Service
	role       *role.Service
}

// initServices constructs the domain services. agentAuth gives the agent
// service the enrollment secret that closes discovery to unknown agent IDs.
func initServices(store storage.Storage, authModel *kit.Auth, historyCfg history.Config, agentAuth config.AgentAuthConfig, logger zerolog.Logger) (services, error) {
	historySVC, err := history.New(store, historyCfg)
	if err != nil {
		return services{}, err
	}
	return services{
		history:    historySVC,
		access:     access.New(authModel, store, logger),
		credential: credential.New(store, logger),
		session:    session.New(store, logger),
		agent:      agent.New(store, logger, agent.WithEnrollSecret(agentAuth.EnrollSecret)),
		spec:       spec.New(store, logger),
		role:       role.New(store, logger),
		user:       user.New(store, logger),
	}, nil
}

// buildCluster assembles the storage + leadership + event-hub trio declared
// by cfg. In raft mode the hub is also wired to replicate mutations through
// the Raft log. Returns the store/leadership/hub the rest of the app uses,
// plus a shutdown function for the storage resources (nil for ephemeral standalone).
func buildCluster(cfg cluster.Config, logger zerolog.Logger) (storage.Storage, cluster.Leadership, *event.Hub, func(), error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("cluster: %w", err)
	}
	inner := inmemory.New()
	hub := event.NewHub(logger)
	switch cfg.Backend {
	case "", cluster.BackendStandalone:
		if cfg.Ephemeral() {
			logger.Warn().Msg("standalone.ephemeral=true: control-plane state will be lost on restart")
			return inner, standalone.NewLeadership(), hub, nil, nil
		}
		store, err := durable.Open(cfg.Standalone.DataDir)
		if err != nil {
			hub.Close()
			return nil, nil, nil, nil, err
		}
		logger.Info().Str("data_dir", cfg.Standalone.DataDir).Msg("standalone durable storage restored")
		return store, standalone.NewLeadership(), hub, func() {
			if err := store.Close(); err != nil {
				logger.Error().Err(err).Msg("close standalone storage")
			}
		}, nil
	case cluster.BackendRaft:
		disco, err := buildDiscovery(cfg.Discovery)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		p, err := raftpkg.New(raftpkg.Config{
			NodeID:           cfg.Raft.NodeID,
			BindAddr:         cfg.Raft.BindAddr,
			AdvertiseAddr:    cfg.Raft.AdvertiseAddr,
			DataDir:          cfg.Raft.DataDir,
			ElectionTimeout:  msToDuration(cfg.Raft.ElectionTimeoutMs),
			HeartbeatTimeout: msToDuration(cfg.Raft.HeartbeatTimeoutMs),
		}, inner, hub, disco, logger)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return p.Store(), p.Leadership(), hub, func() { _ = p.Shutdown() }, nil
	default:
		return nil, nil, nil, nil, fmt.Errorf("cluster: unknown backend %q", cfg.Backend)
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

// bootstrapOptions tells bootstrap where this node keeps its state, which is
// where a generated administrator password goes: a file in the data directory,
// or the log when the state is explicitly ephemeral. It also passes the
// password the operator chose.
func bootstrapOptions(cfg config.Config) bootstrap.Options {
	return bootstrap.Options{
		DataDir:       cfg.Cluster.DataDir(),
		Ephemeral:     cfg.Cluster.Ephemeral(),
		AdminPassword: cfg.BootstrapAdminPassword,
	}
}

// readyForBootstrap distinguishes leader election from applied-state readiness.
// Followers skip seeding; leaders must finish replay before reading seed data.
func readyForBootstrap(ctx context.Context, leader cluster.Leadership, timeout time.Duration) (bool, error) {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !waitForLeader(readyCtx, leader) || !leader.AmLeader() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := cluster.AwaitLeaderReady(readyCtx, leader); err != nil {
		return false, fmt.Errorf("cluster: wait for leader state replay: %w", err)
	}
	return true, nil
}

// waitForLeader polls until a leader is elected or ctx expires. Election is
// followed by AwaitLeaderReady before any bootstrap reads on the leader.
func waitForLeader(ctx context.Context, l cluster.Leadership) bool {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return false
		}
		if l.AmLeader() || l.CurrentLeader() != "" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// buildMainHandler wires the UI and API listener. resolver gives client IPs to
// the login limiter and the stream limiter, and tells the auth and device
// cookies whether the client used HTTPS. The listener has no limit on failed authentication:
// its tokens and session IDs cannot be guessed, and passwords go through the
// login limiter. Cross-origin browser mutations are refused before any route.
func buildMainHandler(cfg config.Config, logger zerolog.Logger, svc services, authModel *kit.Auth, proxyPool *proxy.Pool, eventHub *event.Hub, leadership cluster.Leadership, resolver *clientip.Resolver) (http.Handler, *loghub.Hub) {
	var (
		secure                 = cookieSecure(cfg.Auth.CookieSecure, resolver)
		logHub                 = buildLogHub(logger, svc.agent, proxyPool, cfg.Streams)
		streamLimit            = middleware.NewStreamLimiter(cfg.Streams.MaxPerIP, resolver)
		logAccess, eventAccess = streamChecks(authModel, logger)
		apiHandler             = handler.NewAPI(logger, svc.user, svc.access, svc.session, svc.credential, svc.agent, svc.spec, svc.history, proxyPool, eventHub, logHub, streamLimit, logAccess)
		authMW                 = middleware.Auth(authModel.Verifier, authModel.Session, secure, logger)
		optAuthMW              = middleware.OptionalAuth(authModel.Verifier, authModel.Session, secure, logger)
		uiHandler              = handler.NewUI(logger, svc.access, svc.spec, eventHub, resolver, secure)
		staticHandler          = handler.NewStatic(logger)
		logMW                  = middleware.Logger(logger)
		ridMW                  = middleware.RequestID()
		mux                    = http.NewServeMux()

		permMW = route.PermMW(func(p enum.Permission) route.BaseMW {
			return middleware.RequirePermission(p)
		})
	)
	apiHandler.Routes(mux, authMW, permMW, ridMW, logMW)
	uiHandler.Routes(mux, authMW, optAuthMW, permMW)
	staticHandler.Routes(mux)

	route.HandleFunc(mux, routepath.ApiEventStream, eventHub.SSEHandler(eventAccess), ridMW, logMW, authMW, middleware.StreamLimit(streamLimit))

	var h http.Handler = mux
	h = middleware.CrossOrigin(cfg.CORS, logger)(h)
	h = middleware.Negotiate(responder.NewJSON(), responder.NewHTML())(h)
	h = middleware.Leader(leadership, middleware.LeaderOptions{
		ForwardPort: addrPort(cfg.HTTP.Addr),
		TargetTLS:   cfg.TLS.Server.Enabled(),
	})(h)
	h = middleware.CORS(cfg.CORS)(h)
	h = middleware.Recovery(logger)(h)
	return h, logHub
}

// streamChecks returns the access re-checks of the two long-lived streams, built
// on the session service Auth uses: the task log stream needs AgentsGet, like
// its route; the event stream needs authentication only, like its route. Each
// stream runs its check every keepalive interval and ends when it fails.
func streamChecks(authModel *kit.Auth, logger zerolog.Logger) (logs, events func(*http.Request) error) {
	return middleware.Recheck(authModel.Session, enum.AgentsGet, logger), middleware.Recheck(authModel.Session, "", logger)
}

// cookieSecure decides whether the auth and device cookies of a response carry
// the Secure attribute: always with auth.cookie_secure "always", otherwise when
// the client used HTTPS, directly or through a trusted proxy.
func cookieSecure(mode string, resolver *clientip.Resolver) func(*http.Request) bool {
	always := mode == auth.CookieSecureAlways
	return func(r *http.Request) bool {
		return always || resolver.HTTPS(r)
	}
}

// warnOpenDiscovery logs who can register agents through the discovery
// listeners. agent_auth.require authenticates enrolled agents only: the first
// sync of an unknown agent ID enrolls its token and sets the endpoint podium
// calls. Besides the network, only an enrollment secret
// (SOLTI_AGENT_AUTH_ENROLL_SECRET) or client certificates
// (tls.server.client_ca_file) keep other hosts out of enrollment.
func warnOpenDiscovery(logger zerolog.Logger, cfg config.Config) {
	switch {
	case !cfg.AgentAuth.Require:
		logger.Warn().Msg("agent authentication disabled: any host that reaches the discovery listeners can register agents")
	case cfg.AgentAuth.EnrollSecret == "" && cfg.TLS.Server.ClientCAFile == "":
		logger.Warn().
			Str("http_discovery_addr", cfg.HTTPDiscovery.Addr).
			Str("grpc_addr", cfg.GRPC.Addr).
			Msg("agent authentication covers enrolled agents only: the first sync of an unknown agent ID enrolls it and sets the endpoint podium calls, so set SOLTI_AGENT_AUTH_ENROLL_SECRET or tls.server.client_ca_file, or keep the discovery listeners reachable only from agent networks")
	}
}

// buildDiscoveryListeners wires the HTTP and gRPC discovery listeners around one
// failure limiter: both count agent token failures per (client IP, agent ID), so
// an agent has one budget whichever transport it uses.
func buildDiscoveryListeners(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, leadership cluster.Leadership, cfg config.Config, resolver *clientip.Resolver, serverTLS *tls.Config) (http.Handler, *grpc.Server) {
	limiter := kit.NewLimiter(cfg.Auth)
	return buildDiscoveryHandler(logger, agentSVC, eventHub, leadership, addrPort(cfg.HTTPDiscovery.Addr), limiter, resolver, cfg.AgentAuth.Require, cfg.TLS.Server.Enabled()),
		buildGRPCServer(logger, agentSVC, eventHub, leadership, limiter, serverTLS, cfg.AgentAuth.Require)
}

// buildDiscoveryHandler wires the HTTP discovery listener. It has no per-IP
// limit: the handler counts authentication failures per (client IP, agent ID)
// in limiter, so agents behind one egress IP never block each other.
func buildDiscoveryHandler(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, leadership cluster.Leadership, httpPort int, limiter *ratelimit.Limiter, resolver *clientip.Resolver, requireAuth bool, targetTLS bool) http.Handler {
	var (
		httpDiscovery = handler.NewHTTPDiscovery(logger, agentSVC, eventHub, limiter, resolver, requireAuth)
		mux           = http.NewServeMux()
	)
	mux.HandleFunc("/api/v1/discovery/sync", httpDiscovery.Sync)

	var h http.Handler = mux
	h = middleware.Leader(leadership, middleware.LeaderOptions{ForwardPort: httpPort, TargetTLS: targetTLS})(h)
	h = middleware.Recovery(logger)(h)
	h = middleware.Logger(logger)(h)
	h = middleware.RequestID()(h)
	return h
}

// buildLogHub wires the (agent, task) log fanout hub. The OpenFunc resolves
// the agent, gets its proxy, and opens the underlying StreamTaskLogs — all
// inside a closure so the hub itself stays decoupled from agent/proxy.
func buildLogHub(logger zerolog.Logger, agentSVC *agent.Service, proxyPool *proxy.Pool, cfg middleware.StreamsConfig) *loghub.Hub {
	open := func(ctx context.Context, agentID, taskID, taskUID string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		ag, err := agentSVC.Get(ctx, agentID)
		if err != nil {
			return nil, err
		}
		if ag.Endpoint() == "" {
			return nil, fmt.Errorf("agent %q has no endpoint", agentID)
		}
		token, err := agentSVC.AgentToken(ctx, agentID)
		if err != nil {
			return nil, err
		}
		p, err := proxyPool.Get(ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
		if err != nil {
			return nil, err
		}
		if taskUID != "" {
			pinned, ok := p.(proxy.TaskLogStreamer)
			if !ok {
				return nil, fmt.Errorf("agent proxy cannot pin task UID")
			}
			return pinned.StreamTaskLogsForUID(ctx, taskID, taskUID)
		}
		return p.StreamTaskLogs(ctx, taskID)
	}
	return loghub.NewIdentity(open, loghub.Options{SubscriberBuffer: cfg.SubscriberBuffer, Logger: logger})
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

// buildGRPCServer wires the gRPC discovery listener. Like HTTP discovery, it
// has no per-IP limit: the Sync handler counts authentication failures per
// (peer IP, agent ID) in limiter.
func buildGRPCServer(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, leadership cluster.Leadership, limiter *ratelimit.Limiter, serverTLS *tls.Config, requireAuth bool) *grpc.Server {
	// Write methods (must run on leader). Sync is the only agent-facing
	// mutation; everything else is pure read.
	writeMethods := map[string]struct{}{
		discoverv1.DiscoverService_Sync_FullMethodName: {},
	}
	isWrite := func(full string) bool { _, ok := writeMethods[full]; return ok }

	opts := []grpc.ServerOption{
		// Same message and header caps as the HTTP sync; bounded streams, pings and idle connections.
		grpc.MaxRecvMsgSize(handler.MaxSyncBytes),
		grpc.MaxHeaderListSize(16 << 10),
		grpc.MaxConcurrentStreams(256),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second}),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 5 * time.Minute}),
		grpc.ChainUnaryInterceptor(
			interceptor.UnaryRecovery(logger),
			interceptor.UnaryRequestID(),
			interceptor.UnaryLogger(logger),
			interceptor.UnaryLeader(leadership, interceptor.LeaderOptions{IsWrite: isWrite}),
		),
		grpc.ChainStreamInterceptor(
			interceptor.StreamRecovery(logger),
			interceptor.StreamRequestID(),
			interceptor.StreamLogger(logger),
			interceptor.StreamLeader(leadership, interceptor.LeaderOptions{IsWrite: isWrite}),
		),
	}
	if serverTLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}

	var (
		srv           = grpc.NewServer(opts...)
		grpcDiscovery = handler.NewGRPCDiscovery(logger, agentSVC, eventHub, limiter, requireAuth)
	)
	discoverv1.RegisterDiscoverServiceServer(srv, grpcDiscovery)
	return srv
}
