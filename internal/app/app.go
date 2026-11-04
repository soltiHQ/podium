// Package app is the composition root for the podium control-plane binary.
//
// It assembles cluster state, services, transports, and background runners
// into a single [App] aggregate with a clean Build / Run / Shutdown contract.
package app

import (
	"context"
	"sync"

	"github.com/soltiHQ/control-plane/internal/loghub"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/grpcserver"
	historyrunner "github.com/soltiHQ/control-plane/internal/server/runner/history"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/lifecycle"
	syncrunner "github.com/soltiHQ/control-plane/internal/server/runner/sync"
	clustersvc "github.com/soltiHQ/control-plane/internal/service/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/uikit/htmx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"

	pageSystem "github.com/soltiHQ/control-plane/ui/templates/page/system"
)

// App is the fully wired control-plane runtime.
type App struct {
	logger zerolog.Logger

	store        storage.Storage
	leadership   cluster.Leadership
	eventHub     *event.Hub
	raftShutdown func()
	proxyPool    *proxy.Pool
	authModel    *kit.Auth

	server       *server.Server
	logHub       *loghub.Hub
	shutdownOnce sync.Once
	shutdownDone chan struct{}
}

// New assembles all dependencies. Standalone seeds the initial data here. A
// raft node waits for no leader: it checks what a seed needs, and once it runs,
// seeds at the start of each of its leadership terms (see [seedingRunner]).
// Returned [App] is ready to [Run]. Caller must call [Shutdown] to release
// deferred resources (Raft node, proxy pool, event hub).
func New(ctx context.Context, cfg config.Config, logger zerolog.Logger) (*App, error) {
	raftMode := cfg.Cluster.Backend == cluster.BackendRaft
	// A raft node cannot know at start whether a term of its own will seed: it
	// checks before anything starts what any seed needs, which would otherwise
	// fail at every term it leads.
	if raftMode {
		if err := bootstrapOptions(cfg).Check(); err != nil {
			return nil, err
		}
	}

	store, leadership, eventHub, raftShutdown, admin, err := buildCluster(cfg.Cluster, logger, hubOptions(cfg)...)
	if err != nil {
		return nil, err
	}

	authModel := kit.New(store, cfg.Auth)
	svc, err := initServices(store, authModel, cfg.History.Archive, cfg.AgentAuth, logger)
	if err != nil {
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}
	// A raft node serves the membership API through its cluster administration;
	// standalone has none, and registers no cluster route.
	if admin != nil {
		svc.cluster = clustersvc.New(admin, logger)
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
	// forwards a write. All opt-in (nil = plaintext).
	serverTLS, err := cfg.TLS.Server.Build()
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

	lifecycleRunner, err := lifecycle.New(cfg.Lifecycle, logger, store, eventHub, leadership)
	if err != nil {
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	syncRunner, err := syncrunner.New(cfg.Sync, logger, store, proxyPool, eventHub, svc.history, leadership)
	if err != nil {
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	historyRunner, err := historyrunner.New(cfg.History.Collector, logger, store, proxyPool, svc.history, leadership)
	if err != nil {
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	mainHandler, logHub := buildMainHandler(cfg, logger, svc, authModel, proxyPool, eventHub, leadership, resolver, clientTLS)
	httpCfg := cfg.HTTP
	httpCfg.TLSConfig = uiServerTLS
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
	discoveryHandler, grpcSrv := buildDiscoveryListeners(logger, svc.agent, eventHub, leadership, cfg, resolver, serverTLS, clientTLS, forwarder)
	discoveryCfg := cfg.HTTPDiscovery
	discoveryCfg.TLSConfig = serverTLS
	httpDiscoveryRunner, err := httpserver.New(discoveryCfg, logger, discoveryHandler)
	if err != nil {
		logHub.Close()
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	grpcRunner, err := grpcserver.New(cfg.GRPC, logger, grpcSrv)
	if err != nil {
		logHub.Close()
		_ = proxyPool.Close()
		closeOnError(raftShutdown, eventHub)
		return nil, err
	}

	runners := []server.Runner{httpRunner, httpDiscoveryRunner, grpcRunner, lifecycleRunner, syncRunner, historyRunner}
	if raftMode {
		runners = append(runners, seedingRunner(cfg, logger, leadership, svc))
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
		eventHub:     eventHub,
		raftShutdown: raftShutdown,
		proxyPool:    proxyPool,
		authModel:    authModel,
		server:       srv, logHub: logHub, shutdownDone: make(chan struct{}),
	}, nil
}

// Run starts the server runners and blocks until ctx is cancelled or a runner
// exits unexpectedly. Returns the first error encountered, if any.
func (a *App) Run(ctx context.Context) error {
	return a.server.Run(ctx)
}

// Shutdown releases resources acquired by [New] in reverse order of creation.
// Bounded by ctx — if any step (Raft fsync, hub drain, pool close) hangs
// past the deadline, Shutdown returns and the process exits dirty. The
// caller logs that case; subsequent calls are no-ops.
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
