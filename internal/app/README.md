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
  all runners (HTTP, gRPC, sync, lifecycle, history, and in raft mode
  bootstrap).
- `Shutdown` closes resources in reverse acquisition order: proxy pool →
  event hub → Raft node. Safe to call multiple times. In raft mode the Raft
  node's shutdown first closes the gRPC forwarder of a follower's syncs, its
  connection to the leader included, and the lookups of the raft hosts that
  the client IP resolver trusts: both live as long as the node.

## Startup flow
```text
  app.New(ctx, cfg, logger)
        │
        ▼
  Options.Check()                 ← raft only: what any seed needs, before anything starts
        │
        ▼
  buildCluster()                  ← store + leadership + event-hub (+ Raft node if cfg.Cluster.Backend=raft)
        │
        ▼
  kit.New(store, cfg.Auth)        ← auth composition root (JWT, sessions, RBAC, rate-limit)
        │
        ▼
  initServices()                  ← user, role, credential, session, agent, spec, access
        │
   ┌────┴───────────────┬────────────────────────────────────────┐
   │ standalone         │ raft                                   │
   │ bootstrap.Run()    │ no wait: the bootstrap runner seeds at │
   │                    │ the start of each leadership term      │
   └────┬───────────────┴────────────────────────────────────────┘
        │
        ▼
  proxy.NewPool()                 ← outbound client pool (HTTP + gRPC) to agents
        │
        ▼
  htmx.Configure() + hooks        ← global UI ↔ transport wiring (errorPage, loginPath)
        │
        ▼
  build runners                   ← lifecycle, sync, history, http, http-discovery, grpc (+ bootstrap if raft)
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
   │ • a.Shutdown()  →  proxy.Close → eventHub.Close → raftShutdown
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
