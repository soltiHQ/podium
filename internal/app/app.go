// Package app is the composition root for the podium control-plane binary.
//
// It assembles cluster state, services, transports, and background runners
// into a single [App] aggregate with a clean Build / Run / Shutdown contract.
package app

import (
	"context"
	"crypto/x509"
	"sync"
	"time"

	"github.com/soltiHQ/control-plane/internal/loghub"

	"github.com/rs/zerolog"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	authcred "github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/buildinfo"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/metrics"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/grpcserver"
	historyrunner "github.com/soltiHQ/control-plane/internal/server/runner/history"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/lifecycle"
	"github.com/soltiHQ/control-plane/internal/server/runner/ops"
	syncrunner "github.com/soltiHQ/control-plane/internal/server/runner/sync"
	clustersvc "github.com/soltiHQ/control-plane/internal/service/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"

	pageSystem "github.com/soltiHQ/control-plane/ui/templates/page/system"
)

// options are the settings of [New] that production never changes.
type options struct {
	passwordCost int
}

// defaultOptions are the production settings.
func defaultOptions() options {
	return options{passwordCost: authcred.DefaultBcryptCost}
}

// Option changes how [New] assembles the App.
type Option func(*options)

// WithPasswordCost sets the bcrypt cost of stored passwords and of the login
// dummy hash. The cost is 12 and operators cannot change it; this option is
// for tests, which pass bcrypt.MinCost to keep logins fast.
func WithPasswordCost(cost int) Option {
	return func(o *options) { o.passwordCost = cost }
}

// App is the fully wired control-plane runtime.
type App struct {
	logger zerolog.Logger

	store        storage.Storage
	leadership   cluster.Leadership
	cluster      *clustersvc.Service // a test hook: only tests read it; the raft node's cluster service, nil in standalone
	eventHub     *event.Hub
	raftShutdown func()
	proxyPool    *proxy.Pool
	authModel    *kit.Auth

	server        *server.Server
	logHub        *loghub.Hub
	ready         *readiness
	grpcHealth    *grpcHealth
	shutdownDelay time.Duration
	// sleep waits d, or until ctx ends; New sets it, and a test replaces it.
	sleep func(ctx context.Context, d time.Duration)
	// listeners are the runners that bind a port: the app is ready once all are
	// bound. ui and grpc are two of them, which tests dial. ops is the ops runner,
	// nil when ops.addr is empty; opsHandler, which serves it, takes the handlers
	// that other parts add to the listener.
	listeners    []listener
	ui           *httpserver.Runner
	grpc         *grpcserver.Runner
	ops          *httpserver.Runner
	opsHandler   *ops.Handler
	shutdownOnce sync.Once
	shutdownDone chan struct{}
}

// New assembles all dependencies. Standalone seeds the initial data here. A
// raft node waits for no leader: it checks what a seed needs, and once it runs,
// seeds at the start of each of its leadership terms (see [seedingRunner]).
// Returned [App] is ready to [Run]. Caller must call [Shutdown] to release
// deferred resources (Raft node, proxy pool, event hub).
func New(ctx context.Context, cfg config.Config, logger zerolog.Logger, opts ...Option) (*App, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	raftMode := cfg.Cluster.Backend == cluster.BackendRaft
	var hooks probeHooks
	m := metrics.New(buildinfo.Read())
	clusterOpts := []clusterOption{withHubOptions(hubOptions(cfg)...), withProbes(&hooks), withMetrics(m)}
	// A raft node digests the settings that every replica must share, answers
	// its peers' configuration requests with the digests, and checks them
	// against its leader's (raftpkg.Admin.ConfigCheck). Standalone does neither.
	var (
		digests cluster.Digests
		// clientCAs is the agents' CA bundle that a raft node reads once, for its
		// digests and for the pool of its discovery listeners (agentListenerTLS).
		clientCAs []*x509.Certificate
	)
	if raftMode {
		// A raft node cannot know at start whether a term of its own will seed:
		// it checks before anything starts what any seed needs, which would
		// otherwise fail at every term it leads.
		if err := bootstrapOptions(cfg).Check(); err != nil {
			return nil, err
		}
		cas, err := agentCAs(cfg)
		if err != nil {
			return nil, err
		}
		clientCAs = cas
		shared, err := sharedSettings(cfg, clientCAs)
		if err != nil {
			return nil, err
		}
		digests = shared.Digests()
		clusterOpts = append(clusterOpts, withConfigDigests(digests))
	}

	store, leadership, eventHub, raftShutdown, admin, err := buildCluster(cfg.Cluster, logger, clusterOpts...)
	if err != nil {
		return nil, err
	}

	m.Inventory(store)
	authModel := kit.New(store, cfg.Auth, o.passwordCost)
	svc, err := initServices(store, authModel, cfg.History.Archive, cfg.AgentAuth, logger, o.passwordCost)
	if err != nil {
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}
	// A raft node serves the membership API through its cluster administration,
	// and its status names the fingerprint of the settings every replica must
	// share; standalone has none, and registers no cluster route.
	if admin != nil {
		svc.cluster = clustersvc.New(admin, logger, clustersvc.WithFingerprint(digests.Fingerprint()))
	}

	// Default users/roles/credentials seeding must run through the store, on the
	// leader. Standalone always leads: it seeds here, and a seed that fails fails
	// the start. A raft node leads in some terms only; its seeding runner seeds
	// at the start of each, and the seed replicates to the other nodes.
	if !raftMode {
		if err = bootstrap.Run(ctx, logger, bootstrapOptions(cfg), svc.role, svc.user, svc.credential); err != nil {
			closeOnError(raftShutdown, eventHub)
			return nil, err
		}
	}

	// TLS: agent-facing listeners (HTTP-discovery, gRPC) get the full server config,
	// including mTLS when a client CA is set. The human-facing UI gets the SAME cert
	// but WITHOUT a client-certificate requirement, so enabling mTLS for agents does
	// not lock browsers (which have no client cert) out of the dashboard. One client
	// config for outbound calls: to agents, and from a follower to the leader when it
	// forwards a write. All opt-in (nil = plaintext). A raft node builds the
	// agent-facing listeners' pool from the bundle it read for its digests.
	serverTLS, err := agentListenerTLS(cfg, raftMode, clientCAs)
	if err != nil {
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}
	uiServerTLS, err := cfg.TLS.Server.BuildNoClientAuth()
	if err != nil {
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}
	clientTLS, err := cfg.TLS.Client.Build()
	if err != nil {
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	// Surface the actual state of BOTH directions so an operator can't silently
	// half-enable TLS (e.g. server on, but CP→agent still plaintext).
	logger.Info().
		Bool("server_tls", cfg.TLS.Server.Enabled()).
		Bool("server_mtls", cfg.TLS.Server.ClientCAFile != "").
		Bool("client_tls", cfg.TLS.Client.Enabled()).
		Bool("client_mtls", cfg.TLS.Client.CertFile != "").
		Msg("tls configuration")
	warnOpenDiscovery(logger, cfg)
	if raftMode {
		warnForwarding(logger, cfg)
	}

	// In raft mode the resolver trusts the hosts of the raft configuration too, so
	// the leader believes the client a follower forwards a write for. Those hosts
	// live as long as the raft node, and close with it.
	resolver, closeHosts, err := clientResolver(cfg, leadership)
	if err != nil {
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}
	raftShutdown = before(closeHosts, raftShutdown)

	proxyPool := proxy.NewPool(clientTLS, cfg.AgentAuth.Require)

	htmx.Configure(cfg.Triggers)

	// UI → transport hooks: plug the templ-based error page and the login
	// redirect path into the generic response/responder layer. Without these
	// calls the binary still works — error HTML bodies are empty and 401
	// responses stay as JSON, which is the correct behaviour when the UI is
	// disabled.
	response.ErrorPageRenderer = pageSystem.ErrorPage
	responder.LoginPath = routepath.PageLogin

	lifecycleRunner, err := lifecycle.New(cfg.Lifecycle, logger, store, proxyPool, eventHub, leadership)
	if err != nil {
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	syncRunner, err := syncrunner.New(cfg.Sync, logger, store, proxyPool, svc.agent, eventHub, svc.history, leadership)
	if err != nil {
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	syncRunner.ObserveTurns(m.SyncTurn)

	historyRunner, err := historyrunner.New(cfg.History.Collector, logger, store, proxyPool, svc.agent, svc.history, leadership)
	if err != nil {
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	// What /readyz and the gRPC health service report; the health service is
	// registered on the gRPC listener below, without auth like the ops routes.
	ready := newReadiness(hooks)
	grpcHealth := newGRPCHealth(ready)
	// The gauge reads the hook that /readyz reads.
	m.StoreFailed(func() bool { return ready.failure != nil && ready.failure() != nil })

	mainHandler, logHub := buildMainHandler(cfg, logger, svc, authModel, proxyPool, eventHub, leadership, resolver, clientTLS, m)
	httpCfg := cfg.HTTP
	httpCfg.TLSConfig = uiServerTLS
	// The streams end as the UI listener starts to shut down, so that one that
	// stays open cannot hold the requests in flight until the timeout. A listener
	// that stops without a signal, because a runner exited, is not ready either.
	httpCfg.OnShutdown = func() {
		ready.shuttingDown.Store(true)
		grpcHealth.sync()
		eventHub.Close()
		logHub.Close()
	}
	httpRunner, err := httpserver.New(httpCfg, logger, mainHandler)
	if err != nil {
		logHub.Close()
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	// A follower forwards its gRPC discovery syncs to the leader over one client
	// connection, which closes with the raft node, after the listeners stopped.
	forwarder := syncForwarder(cfg, leadership, clientTLS)
	if forwarder != nil {
		raftShutdown = before(func() { _ = forwarder.Close() }, raftShutdown)
	}
	discoveryHandler, grpcSrv := buildDiscoveryListeners(logger, svc.agent, eventHub, leadership, cfg, resolver, serverTLS, clientTLS, forwarder, m)
	discoveryCfg := cfg.HTTPDiscovery
	discoveryCfg.TLSConfig = serverTLS
	httpDiscoveryRunner, err := httpserver.New(discoveryCfg, logger, discoveryHandler)
	if err != nil {
		logHub.Close()
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	healthpb.RegisterHealthServer(grpcSrv, grpcHealth)

	grpcCfg := cfg.GRPC
	grpcCfg.OnShutdown = grpcHealth.end
	grpcRunner, err := grpcserver.New(grpcCfg, logger, grpcSrv)
	if err != nil {
		logHub.Close()
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	opsHandler := ops.NewHandler(ops.Probes{
		Ready:   ready.reason,
		Leader:  func() string { return ready.leaderReason(leadership.AmLeader) },
		Version: buildinfo.Read(),
	})
	opsHandler.Handle("GET /metrics", m.Handler())
	listeners := []listener{httpRunner, httpDiscoveryRunner, grpcRunner}
	runners := []server.Runner{httpRunner, httpDiscoveryRunner, grpcRunner, lifecycleRunner, syncRunner, historyRunner}
	var opsRunner *httpserver.Runner
	if cfg.Ops.Enabled() {
		opsRunner, err = httpserver.New(cfg.Ops.HTTP(), logger, opsHandler)
		if err != nil {
			logHub.Close()
			_ = proxyPool.Close()
			closeOnError(raftShutdown, eventHub)
			return nil, err
		}
		listeners = append(listeners, opsRunner)
		runners = append(runners, opsRunner)
	}
	if cfg.AgentAuth.Require {
		runners = append(runners, credentialSweepRunner(logger, leadership, svc.agent))
	}
	if raftMode {
		runners = append(runners, seedingRunner(cfg, logger, leadership, svc))
		// While another node leads, the check compares this node's digests with
		// the leader's, at each change of leader.
		if check := admin.ConfigCheck(logger); check != nil {
			runners = append(runners, check)
		}
	}
	srv, err := server.New(cfg.Server, logger, runners...)
	if err != nil {
		logHub.Close()
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	return &App{
		logger:       logger,
		store:        store,
		leadership:   leadership,
		cluster:      svc.cluster,
		eventHub:     eventHub,
		raftShutdown: raftShutdown,
		proxyPool:    proxyPool,
		authModel:    authModel,
		server:       srv, logHub: logHub, shutdownDone: make(chan struct{}),

		ready: ready, grpcHealth: grpcHealth, shutdownDelay: cfg.Server.ShutdownDelay, sleep: sleepCtx,
		listeners: listeners, ui: httpRunner, grpc: grpcRunner, ops: opsRunner, opsHandler: opsHandler,
	}, nil
}

// listener is a runner that binds a port.
type listener interface {
	// Bound returns a channel that closes once the runner tried to bind, or once
	// a Stop that came before Start has run.
	Bound() <-chan struct{}
	// Addr returns the bound address, "" before the bind, when it failed, or
	// after a Stop that came before Start.
	Addr() string
}

// Run starts the server runners and blocks until ctx is cancelled or a runner
// exits unexpectedly. Returns the first error encountered, if any.
//
// The values of ctx reach the runners; its cancellation and deadline do not, the
// shutdown below stops them.
//
// The cancellation of ctx starts a graceful shutdown: readiness turns to
// "shutting down" and the gRPC health service to NOT_SERVING at once, Run waits
// server.shutdown_delay for the load balancers to notice, and only then do the
// listeners stop accepting. The requests in flight finish within
// server.shutdown_timeout, and the streams end as the UI listener starts to shut
// down.
func (a *App) Run(ctx context.Context) error {
	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	// However Run ends, the node is not ready: a runner that exited on its own
	// ends it without the signal.
	defer func() {
		a.ready.shuttingDown.Store(true)
		a.grpcHealth.sync()
	}()
	go a.awaitBound(runCtx)
	go a.grpcHealth.run(runCtx)
	go a.drain(ctx, runCtx, stop)
	return a.server.Run(runCtx)
}

// awaitBound marks the app ready to serve once every listener is bound, unless
// one fails to bind or ctx ends first.
func (a *App) awaitBound(ctx context.Context) {
	for _, l := range a.listeners {
		select {
		case <-l.Bound():
		case <-ctx.Done():
			return
		}
		if l.Addr() == "" {
			return
		}
	}
	a.ready.bound.Store(true)
	a.grpcHealth.sync()
}

// drain waits for ctx, the shutdown signal, and then reports "not ready" and
// waits out the shutdown delay before it calls stop, which stops the listeners.
// It ends at once when runCtx does, which happens when a runner exits on its own.
func (a *App) drain(ctx, runCtx context.Context, stop context.CancelFunc) {
	select {
	case <-ctx.Done():
	case <-runCtx.Done():
		return
	}
	a.ready.shuttingDown.Store(true)
	a.grpcHealth.sync()
	a.logger.Info().Dur("shutdown_delay", a.shutdownDelay).Msg("shutdown started: not ready, listeners stop after the shutdown delay")
	if a.shutdownDelay > 0 {
		a.sleep(runCtx, a.shutdownDelay)
		if runCtx.Err() != nil {
			return
		}
	}
	stop()
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Shutdown releases the resources [New] acquired, in this order: the log hub,
// the proxy pool, the event hub, then a raft node's gRPC sync forwarder, its
// lookups of the raft hosts' names and the node itself, or the durable store of
// a standalone node that has one. Bounded by ctx — if any step (Raft fsync, hub
// drain, pool close) hangs past the deadline, Shutdown returns and the process
// exits dirty. The caller logs that case; subsequent calls are no-ops.
//
// Returns an error if the deadline elapses before cleanup completes.
func (a *App) Shutdown(ctx context.Context) error {
	a.shutdownOnce.Do(func() {
		if a.shutdownDone == nil {
			a.shutdownDone = make(chan struct{})
		}
		go func() {
			defer close(a.shutdownDone)
			if a.logHub != nil {
				a.logHub.Close()
			}
			if a.proxyPool != nil {
				_ = a.proxyPool.Close()
			}
			if a.eventHub != nil {
				a.eventHub.Close()
			}
			if a.raftShutdown != nil {
				a.raftShutdown()
			}
		}()
	})
	select {
	case <-a.shutdownDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeOnError releases cluster-level resources when [New] aborts mid-build.
func closeOnError(raftShutdown func(), hub *event.Hub) {
	if hub != nil {
		hub.Close()
	}
	if raftShutdown != nil {
		raftShutdown()
	}
}
