# internal/app
Composition root for the podium control-plane binary.
Assembles cluster state, services, transports, and background runners into
a single [`App`] aggregate with a clean **Build / Run / Shutdown** contract.

`cmd/main.go` is a thin entry point: load config → `app.New` → `app.Run` →
`app.Shutdown`. Everything else lives here.

## Package map
```text
app/
├── app.go     App struct, New / Run / Shutdown
├── ready.go   readiness (what /readyz reports) and the gRPC health service
└── build.go   private builders: cluster, services, handlers, runners
```

## App contract
```go
type App struct { /* opaque */ }

func New(ctx, cfg, logger) (*App, error)   // build; standalone seeds here, raft never waits for a leader
func (a *App) Run(ctx) error               // start runners; block until ctx done or runner exits
func (a *App) Shutdown(ctx) error          // release resources; idempotent
```

- `New` returns a ready-to-run [`App`]. Standalone seeds the initial data in
  `New`, and a seed that fails fails it. A raft node waits for no leader: `New`
  checks before anything starts what any seed needs, and the `bootstrap` runner
  seeds at the start of each leadership term of the node, see
  `internal/bootstrap/README.md`. On error `New` releases anything already
  acquired (Raft node, event hub) before returning.
- `Run` delegates to [`server.Server.Run`] which orchestrates lifecycle of
  all runners (HTTP, HTTP discovery, gRPC, the ops listener unless `ops.addr` is
  empty, sync, lifecycle, history; while agent auth is
  required, agent-credentials, which at each start of standalone and each
  leadership term of a raft node converts the stored agent credentials that the
  enrollment secret derives and logs how many are stored or unusable, see
  "Enrollment secret" in `internal/config/README.md`; and in raft mode
  bootstrap and config-check, which compares the settings every replica must
  share with the leader's, see "Configuration drift" in
  `internal/raft/README.md`).
- `Shutdown` closes resources in reverse acquisition order: log hub → proxy
  pool → event hub → in raft mode the gRPC sync forwarder, then the lookups of
  the raft hosts' names that the client IP resolver trusts, then the Raft node.
  Safe to call multiple times.

## Startup flow
```text
  app.New(ctx, cfg, logger)
        │
        ▼
  Options.Check()                 ← raft only: what any seed needs, before anything starts
        │
        ▼
  agentCAs()                      ← raft only: the agents' CA bundle, read once, for the digests and the discovery listeners' pool
        │
        ▼
  sharedSettings().Digests()      ← raft only: the digests of the settings every replica must share
        │
        ▼
  buildCluster()                  ← store + leadership + event-hub (+ Raft node, answering its peers with the digests, and its raftpkg.Admin if cfg.Cluster.Backend=raft)
        │
        ▼
  kit.New(store, cfg.Auth)        ← auth composition root (JWT, sessions, RBAC, rate-limit)
        │
        ▼
  initServices()                  ← user, role, credential, session, agent, spec, access; then cluster over the Admin, with the fingerprint, if raft
        │
   ┌────┴───────────────┬────────────────────────────────────────┐
   │ standalone         │ raft                                   │
   │ bootstrap.Run()    │ no wait: the bootstrap runner seeds at │
   │                    │ the start of each leadership term      │
   └────┬───────────────┴────────────────────────────────────────┘
        │
        ▼
  TLS, warnings                   ← agent-facing TLS (raft: from the bundle agentCAs read); who can register agents; raft only: what the forwards lack
        │
        ▼
  proxy.NewPool()                 ← outbound client pool (HTTP + gRPC) to agents
        │
        ▼
  htmx.Configure() + hooks        ← global UI ↔ transport wiring (errorPage, loginPath)
        │
        ▼
  build runners                   ← lifecycle, sync, history, http, http-discovery, grpc (+ agent-credentials if agent auth is required; + bootstrap and config-check if raft)
        │
        ▼
  server.New(runners…)            ← lifecycle orchestrator
        │
        ▼
  return &App{…}                  ← ready to Run
```

## Runtime flow
```text
  main()
   │
   │ cfg := config.Load()
   │ ctx := signal.NotifyContext(SIGINT, SIGTERM)
   │ a, _ := app.New(ctx, cfg, logger)
   │ defer a.Shutdown()
   │
   ▼
  a.Run(ctx)
        │
        ▼
  server.Server.Run(ctx)          ← spawns each Runner, watches for signal or runner exit
        │
   ┌────┴─────────────────┐
   │ ctx.Done()           │ runner exited
   │ → graceful shutdown  │ → return RunnerExitedError
   └──────────────────────┘
        │
        ▼
  main returns
   │
   │ deferred:
   │ • a.Shutdown()  →  logHub.Close → proxy.Close → eventHub.Close → raftShutdown (forwarder, hosts, raft)
   │ • signal stop()
   ▼
  process exit
```

## Error path during build
If any builder fails inside `New` (e.g. `httpserver.New` cannot bind, Raft
node fails to start, a raft node's seed check or a standalone `bootstrap.Run`
errors), the function releases every resource already acquired before returning
the error. The caller never sees a partially-built `App` — either a fully wired
one or `nil, err`. A raft node's seed at a leadership term fails nothing: its
runner retries it while the node leads.
