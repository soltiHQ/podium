package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
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
	"github.com/soltiHQ/control-plane/internal/server/runner/base"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	clustersvc "github.com/soltiHQ/control-plane/internal/service/cluster"
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
	// cluster is the membership service of a raft node, nil in standalone.
	cluster *clustersvc.Service
}

// initServices constructs the domain services. passwordCost is the bcrypt cost
// of the passwords the credential service stores. agentAuth gives the agent
// service the enrollment secret that closes discovery to unknown agent IDs and
// that derived credentials derive from, and tells it when agent auth is off.
func initServices(store storage.Storage, authModel *kit.Auth, historyCfg history.Config, agentAuth config.AgentAuthConfig, logger zerolog.Logger, passwordCost int) (services, error) {
	historySVC, err := history.New(store, historyCfg)
	if err != nil {
		return services{}, err
	}
	agentOpts := []agent.Option{agent.WithEnrollSecret(agentAuth.EnrollSecret)}
	if !agentAuth.Require {
		agentOpts = append(agentOpts, agent.WithAuthDisabled())
	}
	return services{
		history:    historySVC,
		access:     access.New(authModel, store, logger),
		credential: credential.New(store, logger, passwordCost),
		session:    session.New(store, logger),
		agent:      agent.New(store, logger, agentOpts...),
		spec:       spec.New(store, logger),
		role:       role.New(store, logger),
		user:       user.New(store, logger),
	}, nil
}

// hubOptions are the options of the event hub that cfg asks for. A zero
// events.coalesce keeps the default window, so the hub is never silently left
// without one.
func hubOptions(cfg config.Config) []event.Option {
	return []event.Option{event.WithCoalesce(cfg.Events.WithDefaults().Coalesce)}
}

// clusterOptions are what buildCluster builds with besides its configuration.
type clusterOptions struct {
	// hub are the options of the event hub.
	hub []event.Option
	// digests are the digests of the settings every replica must share, which a
	// raft node answers its peers' configuration requests with and checks
	// against its leader's; nil leaves a raft node without either.
	digests *cluster.Digests
	// probes, when set, is filled with the hooks of the backend that is built.
	probes *probeHooks
}

// clusterOption is an option of buildCluster.
type clusterOption func(*clusterOptions)

// withHubOptions configures the event hub with opts, before the Raft node can
// apply the first event to it.
func withHubOptions(opts ...event.Option) clusterOption {
	return func(o *clusterOptions) { o.hub = append(o.hub, opts...) }
}

// withConfigDigests gives a raft node d, the digests of the settings every
// replica must share (raftpkg.Config.ConfigDigests). Standalone ignores them.
func withConfigDigests(d cluster.Digests) clusterOption {
	return func(o *clusterOptions) { o.digests = &d }
}

// buildCluster assembles the storage + leadership + event-hub trio declared
// by cfg. In raft mode the hub is also wired to replicate mutations through
// the Raft log. opts configure the hub, and a raft node's configuration
// digests. Returns the store/leadership/hub the rest of the app uses,
// plus a shutdown function for the storage resources (nil for ephemeral standalone)
// and, in raft mode, the node's cluster administration, which the cluster service
// changes the membership through and which gives the node's configuration check
// (nil in standalone).
func buildCluster(cfg cluster.Config, logger zerolog.Logger, opts ...clusterOption) (storage.Storage, cluster.Leadership, *event.Hub, func(), *raftpkg.Admin, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("cluster: %w", err)
	}
	var o clusterOptions
	for _, opt := range opts {
		opt(&o)
	}
	inner := inmemory.New()
	hub := event.NewHub(logger, o.hub...)
	switch cfg.Backend {
	case "", cluster.BackendStandalone:
		if cfg.Ephemeral() {
			logger.Warn().Msg("standalone.ephemeral=true: control-plane state will be lost on restart")
			return inner, standalone.NewLeadership(), hub, nil, nil, nil
		}
		store, err := durable.Open(cfg.Standalone.DataDir,
			durable.WithSnapshotThreshold(cfg.Standalone.SnapshotThreshold),
			durable.WithFailureHandler(func(err error) {
				logger.Error().Err(err).Str("data_dir", cfg.Standalone.DataDir).
					Msg("standalone durable storage failed: reads and writes fail until restart")
			}),
		)
		if err != nil {
			hub.Close()
			return nil, nil, nil, nil, nil, err
		}
		if store.Migrated() {
			// Once: the migrated file is in format 2 from here on.
			logger.Warn().Str("data_dir", cfg.Standalone.DataDir).
				Msgf("standalone durable storage migrated %s from single-blob to format 2: older versions refuse the migrated file, so rolling back needs a copy made before this start", durable.FileName)
		}
		logger.Info().Str("data_dir", cfg.Standalone.DataDir).Msg("standalone durable storage restored")
		if o.probes != nil {
			o.probes.failure = store.Failure
		}
		return store, standalone.NewLeadership(), hub, func() {
			if err := store.Close(); err != nil {
				logger.Error().Err(err).Msg("close standalone storage")
			}
		}, nil, nil
	case cluster.BackendRaft:
		disco, err := discovery.New(cfg.Discovery)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		node := raftNodeConfig(cfg.Raft)
		node.ConfigDigests = o.digests
		p, err := raftpkg.New(node, inner, hub, disco, logger)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		if o.probes != nil {
			o.probes.leader = p.Leadership().CurrentLeader
		}
		return p.Store(), p.Leadership(), hub, func() { _ = p.Shutdown() }, p.Admin(), nil
	default:
		return nil, nil, nil, nil, nil, fmt.Errorf("cluster: unknown backend %q", cfg.Backend)
	}
}

// raftNodeConfig is the configuration of the raft node that cfg describes.
func raftNodeConfig(cfg cluster.RaftConfig) raftpkg.Config {
	return raftpkg.Config{
		NodeID:                 cfg.NodeID,
		BindAddr:               cfg.BindAddr,
		AdvertiseAddr:          cfg.AdvertiseAddr,
		DataDir:                cfg.DataDir,
		ElectionTimeout:        msToDuration(cfg.ElectionTimeoutMs),
		HeartbeatTimeout:       msToDuration(cfg.HeartbeatTimeoutMs),
		SnapshotInterval:       cfg.SnapshotInterval,
		SnapshotThreshold:      entries(cfg.SnapshotThreshold),
		TrailingLogs:           entries(cfg.TrailingLogs),
		TLS:                    cfg.TLS,
		AllowInsecureTransport: cfg.AllowInsecureTransport,
		Join:                   cfg.Join,
	}
}

func msToDuration(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// entries converts a number of log entries; zero or less leaves raft.New its own
// default.
func entries(n int) uint64 {
	if n <= 0 {
		return 0
	}
	return uint64(n)
}

// agentCAs reads the agents' CA bundle, tls.server.client_ca_file, as
// tlsconf.ServerConfig.Build reads it, and fails as Build fails on it, with its
// error: a bundle that cannot be read, or holds no certificate, fails the start
// of a raft node here, before the node starts, as it fails a standalone start
// in Build. It gives no certificates while the discovery listeners require no
// client certificate. A raft node reads the bundle once, with this, and both
// digests it (sharedSettings) and builds the discovery listeners' pool from
// what it read (agentListenerTLS), so the node's digest names the very
// certificates its listeners verify agents against.
func agentCAs(cfg config.Config) ([]*x509.Certificate, error) {
	cas, err := cfg.TLS.Server.ClientCAs()
	if err != nil {
		return nil, fmt.Errorf("tls server: client CA: %w", err)
	}
	return cas, nil
}

// agentListenerTLS is the TLS of the agent-facing listeners, HTTP discovery and
// gRPC: in raft mode built from cas, the agents' CA bundle that New read for the
// digests (agentCAs), so the bundle is read once, however the file changes in
// between; in standalone from the file, by Build, as before.
func agentListenerTLS(cfg config.Config, raftMode bool, cas []*x509.Certificate) (*tls.Config, error) {
	if raftMode {
		return cfg.TLS.Server.BuildWithClientCAs(cas)
	}
	return cfg.TLS.Server.Build()
}

// sharedSettings are the settings of cfg that every replica of a raft cluster
// must share (cluster.SharedSettings), as the node applies them: auth with its
// defaults, trusted_proxies as the client IP resolver parses them, cors with
// the defaults that the CORS middleware fills in, and cas, the agents' CA
// bundle as agentCAs read it and the discovery listeners' pool holds it.
// cluster.Fingerprint cannot take cfg itself: config imports cluster.
func sharedSettings(cfg config.Config, cas []*x509.Certificate) (cluster.SharedSettings, error) {
	proxies, err := clientip.ParseTrusted(cfg.TrustedProxies)
	if err != nil {
		return cluster.SharedSettings{}, fmt.Errorf("trusted_proxies: %w", err)
	}
	agentCAs := make([][]byte, 0, len(cas))
	for _, ca := range cas {
		agentCAs = append(agentCAs, ca.Raw)
	}
	authCfg := cfg.Auth.WithDefaults()
	return cluster.SharedSettings{
		JWTSecret:        authCfg.JWTSecret,
		CookieSecure:     authCfg.CookieSecure,
		Issuer:           authCfg.Issuer,
		Audience:         authCfg.Audience,
		AgentAuthRequire: cfg.AgentAuth.Require,
		EnrollSecret:     cfg.AgentAuth.EnrollSecret,
		TrustedProxies:   proxies,
		CORS:             cluster.CORSSettings(cfg.CORS.WithDefaults()),
		AgentMTLS:        cas != nil,
		AgentCAs:         agentCAs,
	}, nil
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

// seedingRunner is the runner that seeds the shared state on a raft node. At the
// start of each leadership term of the node, once the term's WaitReady saw every
// committed write applied, it runs the whole bootstrap.Run: the built-in roles
// are reconciled to this node's definitions, and the administrator is seeded
// only when the user table is empty or a first seed stopped halfway. It retries
// a failure while the node leads, and leaves a lost term to the next one (see
// [base.NewTermTask]). Its log lines name the node: the one whose line says a
// password was generated holds the password file.
func seedingRunner(cfg config.Config, logger zerolog.Logger, leadership cluster.Leadership, svc services) *base.Runner {
	logger = logger.With().Str("node_id", cfg.Cluster.Raft.NodeID).Logger()
	opts := bootstrapOptions(cfg)
	return base.NewTermTask("bootstrap", leadership, logger, func(ctx context.Context) error {
		return bootstrap.Run(ctx, logger, opts, svc.role, svc.user, svc.credential)
	})
}

// What credentialSweepRunner logs of a sweep (agent.CredentialSweep).
const (
	convertedCredentials = "agent credentials: stored tokens that the enrollment secret derives are now derived credentials: no snapshot or log entry written from now on holds them"
	storedCredentials    = "agent credentials: stored tokens are left, kept raw in podium's state and in its backups: tokens agents chose while no enrollment secret was set, and tokens an earlier secret derived, which stay valid and which that secret still derives; resetting an agent's credential (POST /api/v1/agents/{id}/credential) while an enrollment secret is set moves it to a derived credential"
	otherSecretCreds     = "agent credentials: derived credentials of another enrollment secret than the current one: podium refuses these agents and makes no call to them until the secret they derive from is set again, each is re-provisioned with the token podium agent-token prints for it under the current secret, or its credential is reset (POST /api/v1/agents/{id}/credential), which issues a token derived under the current secret"
	noSecretCreds        = "agent credentials: derived credentials and no enrollment secret is set: podium refuses these agents and makes no call to them until the secret they derive from is set again, or each credential is reset (POST /api/v1/agents/{id}/credential), which issues a stored token"
)

// credentialSweepRunner is the runner that sweeps the agent credentials
// (agent.Service.SweepCredentials) at the start of each leadership term of this
// replica, through Leadership.WhenLeader: once at start in standalone, which
// always leads, and once per term on a raft leader, a leader write like any
// other. It converts the stored credentials whose token the enrollment secret
// derives at generation 0, and logs what it found (logCredentialSweep). A
// failure is retried while the term lasts (see [base.NewTermTask]). New runs it
// only while agent auth is required: with agent_auth.require false podium checks
// no token and presents none.
func credentialSweepRunner(logger zerolog.Logger, leadership cluster.Leadership, agents *agent.Service) *base.Runner {
	return base.NewTermTask("agent-credentials", leadership, logger, func(ctx context.Context) error {
		res, err := agents.SweepCredentials(ctx)
		if err != nil {
			return err
		}
		logCredentialSweep(logger, res, agents.EnrollSecretSet())
		return nil
	})
}

// logCredentialSweep logs, with its count, each finding of a sweep: an INFO for
// the stored credentials it converted, a WARN for the stored ones left, whose
// tokens stay in the state and its backups (tokens agents chose without a
// secret, and tokens a former secret derived), and an ERROR for the derived
// ones podium cannot use, which says whether no secret is set (secretSet false)
// or they derive from another one, and every way back. A sweep that found
// nothing logs nothing.
func logCredentialSweep(logger zerolog.Logger, res agent.CredentialSweep, secretSet bool) {
	if res.Converted > 0 {
		logger.Info().Int("converted_credentials", res.Converted).Msg(convertedCredentials)
	}
	if res.Stored > 0 {
		logger.Warn().Int("stored_credentials", res.Stored).Msg(storedCredentials)
	}
	if res.Unusable > 0 {
		msg := otherSecretCreds
		if !secretSet {
			msg = noSecretCreds
		}
		logger.Error().Int("unusable_credentials", res.Unusable).Msg(msg)
	}
}

// buildMainHandler wires the UI and API listener. resolver gives client IPs to
// the login limiter and the stream limiter, and tells the auth and device
// cookies whether the client used HTTPS. The listener has no limit on failed authentication:
// its tokens and session IDs cannot be guessed, and passwords go through the
// login limiter. Cross-origin browser mutations are refused before any route,
// and on a follower before the Leader middleware forwards anything to the
// leader, so a refused request never crosses nodes. A follower forwards writes
// to the leader with forwardTLS, see [leaderOptions]. The cluster routes exist
// only with a cluster service, which a raft node alone has.
func buildMainHandler(cfg config.Config, logger zerolog.Logger, svc services, authModel *kit.Auth, proxyPool *proxy.Pool, eventHub *event.Hub, leadership cluster.Leadership, resolver *clientip.Resolver, forwardTLS *tls.Config) (http.Handler, *loghub.Hub) {
	var apiOpts []handler.APIOption
	if svc.cluster != nil {
		apiOpts = append(apiOpts, handler.WithCluster(svc.cluster))
	}
	var (
		secure                 = cookieSecure(cfg.Auth.CookieSecure, resolver)
		logHub                 = buildLogHub(logger, svc.agent, proxyPool, cfg.Streams)
		streamLimit            = middleware.NewStreamLimiter(cfg.Streams.MaxPerIP, resolver)
		logAccess, eventAccess = streamChecks(authModel, logger)
		apiHandler             = handler.NewAPI(logger, svc.user, svc.access, svc.session, svc.credential, svc.agent, svc.spec, svc.history, proxyPool, eventHub, logHub, streamLimit, logAccess, apiOpts...)
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

	// Outermost first: Recovery, CORS, Negotiate, CrossOrigin, Leader. CrossOrigin
	// answers through the Responder that Negotiate picks, and runs before Leader.
	var h http.Handler = mux
	h = middleware.Leader(leadership, leaderOptions(addrPort(cfg.HTTP.Addr), cfg.TLS.Server.Enabled(), forwardTLS))(h)
	h = middleware.CrossOrigin(cfg.CORS, logger)(h)
	h = middleware.Negotiate(responder.NewJSON(), responder.NewHTML())(h)
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

// warnForwarding logs, once at the start of a raft node, what its forwards to
// the leader lack, one WARN per case; standalone forwards nothing and New calls
// it in raft mode only. A follower forwards the writes it gets, logins, cookies
// and tokens included, and the agents' syncs to the leader:
//   - without tls.server, in plaintext, over HTTP and gRPC alike;
//   - with agent mTLS on (tls.server.client_ca_file) and no tls.client.cert_file,
//     without a client certificate, which the leader's discovery listeners
//     require, so every sync that reaches a follower fails, 502 over HTTP and
//     "leader unreachable" over gRPC.
func warnForwarding(logger zerolog.Logger, cfg config.Config) {
	if !cfg.TLS.Server.Enabled() {
		logger.Warn().
			Msg("raft forwarding runs plaintext: without tls.server a follower forwards the writes it gets, logins, cookies and tokens included, and the agents' syncs to the leader over plain HTTP and gRPC; set tls.server on every node, and tls.client.ca_file to the CA of the nodes' server certificates")
		return
	}
	if cfg.TLS.Server.ClientCAFile != "" && cfg.TLS.Client.CertFile == "" {
		logger.Warn().
			Msg("agent mTLS is on (tls.server.client_ca_file) and tls.client.cert_file is unset: a follower presents no client certificate when it forwards an agent's sync to the leader, whose discovery listeners require one, so every sync that reaches a follower fails; set tls.client.cert_file and key_file to a certificate the agents' CA signed")
	}
}

// buildDiscoveryListeners wires the HTTP and gRPC discovery listeners around one
// failure limiter and one set of sync buckets: both count agent token failures
// per (client IP, agent ID) and charge each sync to the bucket of its agent ID,
// so an agent has one budget of each whichever transport it uses. resolver gives
// both the client IP; gRPC discovery reads it from x-forwarded-for only in raft
// mode, where a follower forwards its syncs to the leader with forwarder, while
// standalone keeps the peer address, as before.
func buildDiscoveryListeners(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, leadership cluster.Leadership, cfg config.Config, resolver *clientip.Resolver, serverTLS, forwardTLS *tls.Config, forwarder *interceptor.Forwarder) (http.Handler, *grpc.Server) {
	limiter := kit.NewLimiter(cfg.Auth)
	syncs := handler.NewSyncBuckets()
	var grpcClients *clientip.Resolver
	if _, raft := leadership.(cluster.Membership); raft {
		grpcClients = resolver
	}
	return buildDiscoveryHandler(logger, agentSVC, eventHub, leadership, addrPort(cfg.HTTPDiscovery.Addr), limiter, syncs, resolver, cfg.AgentAuth.Require, cfg.TLS.Server.Enabled(), forwardTLS),
		buildGRPCServer(logger, agentSVC, eventHub, leadership, limiter, syncs, grpcClients, serverTLS, cfg.AgentAuth.Require, addrPort(cfg.GRPC.Addr), forwarder)
}

// buildDiscoveryHandler wires the HTTP discovery listener. It has no per-IP
// limit: the handler counts authentication failures per (client IP, agent ID)
// in limiter, so agents behind one egress IP never block each other, and charges
// each sync to the bucket of its agent ID in syncs. A follower forwards syncs to
// the leader with forwardTLS, see [leaderOptions].
func buildDiscoveryHandler(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, leadership cluster.Leadership, httpPort int, limiter *ratelimit.Limiter, syncs *ratelimit.Buckets, resolver *clientip.Resolver, requireAuth bool, targetTLS bool, forwardTLS *tls.Config) http.Handler {
	var (
		httpDiscovery = handler.NewHTTPDiscovery(logger, agentSVC, eventHub, limiter, syncs, resolver, requireAuth)
		mux           = http.NewServeMux()
	)
	mux.HandleFunc("/api/v1/discovery/sync", httpDiscovery.Sync)

	var h http.Handler = mux
	h = middleware.Leader(leadership, leaderOptions(httpPort, targetTLS, forwardTLS))(h)
	h = middleware.Recovery(logger)(h)
	h = middleware.Logger(logger)(h)
	h = middleware.RequestID()(h)
	return h
}

// leaderOptions makes a follower forward writes to port on the leader's host:
// over TLS when tls.server is set (targetTLS), with forwardTLS, the tls.client
// configuration, which verifies the leader's certificate against
// tls.client.ca_file and presents tls.client's certificate when it has one.
func leaderOptions(port int, targetTLS bool, forwardTLS *tls.Config) middleware.LeaderOptions {
	return middleware.LeaderOptions{ForwardPort: port, TargetTLS: targetTLS, TLSClientConfig: forwardTLS}
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
		p, err := proxyPool.Get(ag.ID(), ag.Endpoint(), ag.EndpointType(), ag.APIVersion(), token)
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

// buildGRPCServer wires the gRPC discovery listener, which serves on port. Like
// HTTP discovery, it has no per-IP limit: the Sync handler counts authentication
// failures per (client IP, agent ID) in limiter, with the client IP that
// resolver gives (nil: the peer), and charges each sync to the bucket of its
// agent ID in syncs. On a follower, forwarder sends a Sync to the leader's gRPC
// listener, at the host of the leader's raft address and port, which every
// replica is assumed to serve it on; a write it does not forward answers
// Unavailable with that address in the x-leader hint.
func buildGRPCServer(logger zerolog.Logger, agentSVC *agent.Service, eventHub *event.Hub, leadership cluster.Leadership, limiter *ratelimit.Limiter, syncs *ratelimit.Buckets, resolver *clientip.Resolver, serverTLS *tls.Config, requireAuth bool, port int, forwarder *interceptor.Forwarder) *grpc.Server {
	// Write methods (must run on leader). Sync is the only agent-facing
	// mutation; everything else is pure read.
	writeMethods := map[string]struct{}{
		discoverv1.DiscoverService_Sync_FullMethodName: {},
	}
	isWrite := func(full string) bool { _, ok := writeMethods[full]; return ok }
	leaderOpts := interceptor.LeaderOptions{IsWrite: isWrite, Port: port, Forwarder: forwarder}

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
			interceptor.UnaryLeader(leadership, leaderOpts),
		),
		grpc.ChainStreamInterceptor(
			interceptor.StreamRecovery(logger),
			interceptor.StreamRequestID(),
			interceptor.StreamLogger(logger),
			interceptor.StreamLeader(leadership, leaderOpts),
		),
	}
	if serverTLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}

	var (
		srv           = grpc.NewServer(opts...)
		grpcDiscovery = handler.NewGRPCDiscovery(logger, agentSVC, eventHub, limiter, syncs, resolver, requireAuth)
	)
	discoverv1.RegisterDiscoverServiceServer(srv, grpcDiscovery)
	return srv
}

// syncForwarder returns the forwarder of a follower's gRPC discovery syncs in raft
// mode, nil in standalone. It sends a Sync through the generated client, over TLS
// with forwardTLS, the tls.client configuration, when the gRPC listener serves TLS
// (tls.server), as the HTTP listeners forward their writes. The caller closes it.
func syncForwarder(cfg config.Config, leadership cluster.Leadership, forwardTLS *tls.Config) *interceptor.Forwarder {
	if _, raft := leadership.(cluster.Membership); !raft {
		return nil
	}
	return interceptor.NewForwarder(cfg.TLS.Server.Enabled(), forwardTLS, map[string]interceptor.ForwardFunc{
		discoverv1.DiscoverService_Sync_FullMethodName: forwardSync,
	})
}

// forwardSync forwards a Sync with the generated client.
func forwardSync(ctx context.Context, conn grpc.ClientConnInterface, req any, opts ...grpc.CallOption) (any, error) {
	in, ok := req.(*discoverv1.SyncRequest)
	if !ok {
		return nil, fmt.Errorf("forward sync: the request is a %T", req)
	}
	return discoverv1.NewDiscoverServiceClient(conn).Sync(ctx, in, opts...)
}

// clientResolver returns the client IP resolver of cfg, which trusts the
// forwarding headers of trusted_proxies, and in raft mode, where a follower
// forwards writes to the leader, of the hosts of the current raft configuration
// as well (raftHosts). Those hosts' names are looked up until the returned
// function, nil in standalone, closes them.
func clientResolver(cfg config.Config, leadership cluster.Leadership) (*clientip.Resolver, func(), error) {
	membership, raft := leadership.(cluster.Membership)
	if !raft {
		resolver, err := clientip.New(cfg.TrustedProxies)
		return resolver, nil, err
	}
	hosts := clientip.NewHosts(raftHosts(membership))
	resolver, err := clientip.New(cfg.TrustedProxies, clientip.TrustHosts(hosts))
	if err != nil {
		hosts.Close()
		return nil, nil, err
	}
	return resolver, hosts.Close, nil
}

// raftHosts lists the hosts of the members of membership's current configuration,
// as written: the host of each member's raft address.
func raftHosts(membership cluster.Membership) func() []string {
	return func() []string {
		members := membership.Members()
		hosts := make([]string, 0, len(members))
		for _, m := range members {
			if host, _, err := net.SplitHostPort(m.Address); err == nil && host != "" {
				hosts = append(hosts, host)
			}
		}
		return hosts
	}
}

// before returns a function that runs first, then then; either may be nil, and
// so is the result when both are.
func before(first, then func()) func() {
	switch {
	case first == nil:
		return then
	case then == nil:
		return first
	}
	return func() {
		first()
		then()
	}
}
