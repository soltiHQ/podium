# internal/app

Composition root for the podium control-plane binary. Assembles cluster state,
services, transports, and background runners into a single \[`App`\] aggregate
with a clean **Build / Run / Shutdown** contract.

`cmd/main.go` is a thin entry point: it parses the command line, answers
`podium agent-token` and `podium version` itself, runs `podium migrate` through
`app.Migrate` and `podium drop-schema` through `app.DropSchema`, and to serve
builds the loggers and the signal handling around: load config → `app.New` →
`app.Run` → `app.Shutdown`. Everything else lives here.

## Package map

```text
app/
├── app.go       App struct, New / Run / Shutdown
├── ready.go     readiness (what /readyz reports) and the gRPC health service
├── build.go     private builders: cluster, services, handlers, runners
└── database.go  the database mode: its pools, the startup retry, the ping of
                 the primary, the order of its start and shutdown; Migrate
```

## App contract

```go
type App struct { /* opaque */ }

func New(ctx, cfg, logger, opts...) (*App, error) // build; memory seeds here, raft and database never wait for a leader
func (a *App) Run(ctx) error               // start runners; block until ctx done or runner exits
func (a *App) Shutdown(ctx) error          // release resources; idempotent
func Migrate(ctx, cfg, logger, opts...) error // podium migrate: the database mode's schema, and nothing else
func DropSchema(ctx, cfg, logger, wait, opts...) error // podium drop-schema: drops that schema once no replica runs
```

- `New` returns a ready-to-run \[`App`\]. The memory backend seeds the initial
  data in `New`, and a seed that fails fails it. A raft node, or a replica of
  the database mode, waits for no leader: `New` checks before anything starts
  what any seed needs, and the `bootstrap` runner seeds at the start of each
  leadership term of the replica, see `internal/bootstrap/README.md`; on a
  database without an administrator and without
  `SOLTI_BOOTSTRAP_ADMIN_PASSWORD`, `New` fails, naming the variable. In the
  database mode `New` retries the database within the startup retry of 30 s (see
  "Database mode" below). On error `New` releases anything already acquired
  (Raft node or database pools and leadership, event hub) before returning.
- `Run` delegates to \[`server.Server.Run`\] which orchestrates lifecycle of all
  runners (HTTP, HTTP discovery, gRPC, the ops listener unless `ops.addr` is
  empty, sync, lifecycle, history; while agent auth is required,
  agent-credentials, which at each start of a memory node and each leadership
  term of a raft node converts the stored agent credentials that the enrollment
  secret derives and logs how many are stored or unusable, see "Enrollment
  secret" in `internal/config/README.md`; and in raft mode and in the database
  mode bootstrap and config-check, which compares the settings every replica
  must share with the leader's, see "Configuration drift" in
  `internal/raft/README.md`: a raft node fetches the leader's digests over the
  raft port, a database replica reads them from the leader's row of the replica
  registry, through `raftpkg.NewConfigCheck`).
- `New` builds the metrics registry (`internal/metrics`) before the cluster, so
  that the store's commits are timed from the first one, wires the middleware or
  interceptors of the UI, discovery and gRPC listeners and the handlers'
  observers into it, and registers `GET /metrics` on the ops listener; the
  series are listed in "Metrics" of `internal/server/README.md`.
- Cancelling the `ctx` of `Run` starts a graceful shutdown: `/readyz` reports
  "shutting down" and the gRPC health service `NOT_SERVING` at once, `Run` waits
  `server.shutdown_delay`, and only then do the listeners stop accepting (see
  "Shutdown" in `internal/config/README.md`). The event hub and the log hub
  close as the UI listener starts to stop, which ends their streams.
- `Shutdown` closes resources in this order: log hub → proxy pool → event hub →
  in raft mode and in the database mode the gRPC sync forwarder, then the
  lookups of the members' host names that the client IP resolver trusts, then
  the Raft node, or the database mode's ping, events transport, leadership and
  pools. Safe to call multiple times.

## Database mode

`buildCluster` builds the database mode in `database.go` (`buildDatabase`), in
this order:

1. The pools, none of which connects yet: the primary's and, with
   `cluster.database.replica_url`, the replica's, each capped at
   `max_open_conns` with as many idle, and the lease's of one connection, each
   with its `application_name` (`podium`, `podium-replica`, `podium-lease`). The
   URLs reach the driver alone (`openPool`): a URL that does not parse fails
   with an error that names the setting; a URL without `connect_timeout` gets 5
   s.
1. The leadership (`database.New`), with a random replica ID per process, which
   connects to nothing until it runs.
1. Within the startup retry (`reach`): the schema, migrated when
   `cluster.database.migrate` is true (`sqlstore.Migrate`, under the database
   lock), and the store on it (`sqlstore.Open`, which refuses every schema but
   this podium's), with the leadership as its fence. A database it cannot reach
   (`storage.ErrUnavailable`) is tried again a second apart, with one WARN for
   each database read that fails, for 30 s, after which `New` fails and main
   exits 1; any other failure, a schema newer or, without migrations, older than
   this podium's included, fails `New` at once. The window is one for every read
   of the database at start: `New`'s own read of the user table
   (`bootstrap.CheckSeed`) takes what is left of it (`databaseHooks.retry`).
1. The events transport (`database.NewEventTransport`, with the `LISTEN` of
   `podium-events`), which becomes the hub's writer before any event can be
   emitted.
1. The leadership, the transport and the ping of the primary run until
   `Shutdown`; the metrics get the pools, the lease's counters and the dropped
   events, and the readiness the ping's failure.

The ping (`storePing`) pings the primary every 5 s, each ping within 2 s: while
the last one failed, `/readyz` answers 503 `storage failed`,
`podium_store_failed` is 1 and the gRPC health service `NOT_SERVING`; the next
ping that answers restores them. The readiness of the database mode waits for no
leader, and `/leaderz` follows the lease. The forwarding, the trust of the
forwarding replicas' client IPs, the WARN of plaintext forwarding and the
config-check work as in raft, as the leadership is a `cluster.Membership`.

`Shutdown` stops, after the runners that `Run` stopped and waited for, the ping,
then the events transport, whose prune runs under `WhenLeader`, then the
leadership, whose end releases the lease and deletes the replica's registry row,
and closes the pools: the old leader's runners never overlap the new leader's
after a clean stop. A runner that outlived `Run`'s drain deadline
(`server.shutdown_timeout`) is still `WhenLeader` work: the leadership does not
release the lease then, which expires a lease after its last renewal (see "The
lease" in `internal/cluster/database/README.md`). The leadership, the transport
and the ping run on contexts of their own, which no shutdown signal ends before
`Shutdown` does. The seed and the configuration check, whose runners read the
database for themselves, log with the database mode's logger, which names the
primary's host, port and database. The lifecycle, sync and history runners and
the credential sweep read the database through the store, and log with the base
logger, as the listeners do.

`Migrate`, which `podium migrate` runs, opens one connection to the primary
(`podium-migrate`), migrates within the startup retry, logs the host, the port,
the database and the versions before and after, and runs nothing else.

`DropSchema`, which `podium drop-schema` runs, opens one connection to the
primary (`podium-drop-schema`) and drops every table of podium's in one
transaction (`sqlstore.DropSchema`) once no replica runs on the database
(`database.CheckStopped`, which it asks in that transaction, under the lock on
the migrations). While replicas run it waits for them up to its wait, logging
one WARN, each attempt within the startup retry and a pause apart, then fails
with `database.ErrRunning`; with no wait it fails at once. A schema newer than
this podium's fails it at once, and nothing is dropped. It logs the host, the
port, the database, the version before and the tables it dropped, or that there
was no schema to drop, and runs nothing else (`TestDatabaseDropSchema`).

The tests of the database mode run whole apps on Postgres, each on a database of
its own from `pgtest`, and skip without one; the second invocation of
`task ci/golang/test/postgres` runs them: `TestDatabaseModeServes`,
`TestTwoReplicasForwardWrites`, `TestBootstrapNeedsPassword`,
`TestDatabaseOutageRecovers`, `TestDatabaseStartRetries`,
`TestDatabaseURLNeverLeaks`, `TestDatabaseSchemaNotOursStopsTheStart`,
`TestDatabaseReplicaSettingsChecked`, `TestDatabaseStartRetriesTheSeedCheck`,
`TestDatabaseReadyWithoutLeader`, `TestDatabaseFailoverUnderLoad` and
`TestDatabaseDropSchema`. `TestPoolConfig`, which needs no Postgres, pins each
pool's `application_name`, the connect timeout of a URL without one, and the
error of a URL that does not parse. Their startup retry and ping are shortened
(`withDatabaseTimings`), and their leases are 1.5 s, the failover's 4.5 s.

`TestDatabaseFailoverUnderLoad` runs three apps on one database, each through a
TCP proxy of its own, which every pool of the app and its events listener
connect through, with a lease of 4.5 s, whose renewal interval exceeds with a
margin the second between two reads of the lease row by a follower. Simulated
agents sync over HTTP discovery through all three, the followers forwarding to
the leader, each its sequence as its uptime. At a random point of the leader's
renewal interval, its proxy then cuts its connections and refuses new ones, as a
partition cuts it (a clean stop would release the lease). The cut-off leader's
`/leaderz` answers 503 within two thirds of the lease of the cut, and the work
of its leader-only runners ends within that and a scheduling tolerance of 100
ms, the time they take to return once the step-down cancelled them, before the
new leader's starts; the first sync sent after the cut answered 200, through the
new leader, comes within the lease and one renewal interval of it; and the
database holds, for every agent, the sequence of its last acknowledged sync or a
later one: no write answered 200 is lost. The test watches the leader-only
runners' work through a hook that production never sets (`options.leaderWork`):
New runs their `WhenLeader` work through it (`watchedLeadership`), as nothing
else shows that work's end.

## Startup flow

```text
  app.New(ctx, cfg, logger)
        │
        ▼
  Options.Check()                 ← raft and database only: what any seed needs, before anything starts
        │
        ▼
  agentCAs()                      ← raft and database only: the agents' CA bundle, read once, for the digests and the discovery listeners' pool
        │
        ▼
  sharedSettings().Digests()      ← raft and database only: the digests of the settings every replica must share
        │
        ▼
  buildCluster()                  ← store + leadership + event-hub (+ Raft node, answering its peers with the digests, and its raftpkg.Admin if cfg.Cluster.Backend=raft; + the database mode's pools, leadership, store, events transport and ping, within the startup retry, if database)
        │
        ▼
  kit.New(store, cfg.Auth)        ← auth composition root (JWT, sessions, RBAC, rate-limit)
        │
        ▼
  initServices()                  ← history, user, role, credential, session, agent, spec, access; then cluster over the Admin, with the fingerprint, if raft
        │
   ┌────┴───────────────┬────────────────────────────────────────┐
   │ memory             │ raft, database                         │
   │ bootstrap.Run()    │ no wait: the bootstrap runner seeds at │
   │                    │ the start of each leadership term;     │
   │                    │ database: bootstrap.CheckSeed first    │
   └────┬───────────────┴────────────────────────────────────────┘
        │
        ▼
  TLS, warnings                   ← agent-facing TLS (raft, database: from the bundle agentCAs read); who can register agents; raft and database only: what the forwards lack
        │
        ▼
  proxy.NewPool()                 ← outbound client pool (HTTP + gRPC) to agents
        │
        ▼
  htmx.Configure() + hooks        ← global UI ↔ transport wiring (errorPage, loginPath)
        │
        ▼
  build runners                   ← lifecycle, sync, history, http, http-discovery, grpc (+ ops unless ops.addr is empty; + agent-credentials if agent auth is required; + bootstrap and config-check if raft or database)
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
   │ ctx := notifyShutdown()         ← first SIGINT/SIGTERM cancels it; a second exits 1 at once
   │ a, _ := app.New(ctx, cfg, logger)
   │
   ▼
  a.Run(ctx)
        │
        ▼
  readiness off, wait shutdown_delay when ctx is done (drain)
        │
        ▼
  server.Server.Run(runCtx)       ← spawns each Runner, watches for signal or runner exit
        │
   ┌────┴─────────────────┐
   │ ctx.Done()           │ runner exited
   │ → graceful shutdown  │ → return RunnerExitedError,
   │                      │   or RunnerError if it failed
   └──────────────────────┘
        │
        ▼
  a.Shutdown(30s timeout)  →  logHub.Close → proxy.Close → eventHub.Close → raftShutdown (forwarder, hosts, raft or the database mode's ping, transport, leadership, pools; memory: none)
        │
        ▼
  main returns
   │
   │ deferred:
   │ • signal stop()
   ▼
  process exit
```

## Error path during build

If any builder fails inside `New` (e.g. Raft node fails to start, the database
is not reached within the startup retry, a raft node's or a database replica's
seed check or a memory `bootstrap.Run` errors), the function releases every
resource already acquired before returning the error. The caller never sees a
partially-built `App` — either a fully wired one or `nil, err`. A listener binds
its port in `Run`, not in `New`: one that cannot bind ends `Run` with a
`RunnerError`. A raft node's seed at a leadership term fails nothing: its runner
retries it while the node leads.
