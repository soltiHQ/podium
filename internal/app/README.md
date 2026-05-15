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

func New(ctx, cfg, logger) (*App, error)   // build + bootstrap (blocks on leader wait)
func (a *App) Run(ctx) error               // start runners; block until ctx done or runner exits
func (a *App) Shutdown()                   // release resources; idempotent
```

- `New` returns a ready-to-run [`App`] and seeds initial data when this
  replica is the leader. On error it releases anything already acquired
  (Raft node, event hub) before returning.
- `Run` delegates to [`server.Server.Run`] which orchestrates lifecycle of
  all runners (HTTP, gRPC, sync, lifecycle).
- `Shutdown` closes resources in reverse acquisition order: proxy pool →
  event hub → Raft node. Safe to call multiple times.

## Startup flow
```text
  app.New(ctx, cfg, logger)
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
        ▼
  waitForLeader(ctx, 30s)
        │
   ┌────┴───────────────┐
   │ leader             │ follower
   │ bootstrap.Run()    │ skip — leader seeds shared state
   └────┬───────────────┘
        │
        ▼
  proxy.NewPool()                 ← outbound client pool (HTTP + gRPC) to agents
        │
        ▼
  htmx.Configure() + hooks        ← global UI ↔ transport wiring (errorPage, loginPath)
        │
        ▼
  build runners                   ← lifecycle, sync, http, http-discovery, grpc
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
node fails to start, `bootstrap.Run` errors), the function releases every
resource already acquired before returning the error. The caller never
sees a partially-built `App` — either a fully wired one or `nil, err`.
