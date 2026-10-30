# internal/server
Unified lifecycle management for all runtime components of the control plane.

## Package map
```text
server/
├── runner.go       Runner interface (Name / Start / Stop)
├── server.go       Server orchestrator: starts, monitors, shuts down runners
├── config.go       ShutdownTimeout configuration
├── error.go        RunnerError, RunnerExitedError, sentinel errors
│
└── runner/
    ├── grpcserver/  gRPC listener → grpc.Server.Serve
    ├── httpserver/  TCP listener  → http.Server.Serve
    ├── lifecycle/   periodic agent liveness checks (active → … → deleted)
    └── sync/        periodic rollout reconciliation (push specs to agents)
```

## Runner interface
```go
type Runner interface {
    Name()  string
    Start(ctx) error   // blocks until done or ctx cancelled
    Stop(ctx)  error   // graceful shutdown within ctx deadline
}
```

## Orchestration flow
```text
  New(cfg, logger, runners...)
        │
        ▼
  Run(ctx)
        │
   ┌────┴────────────────────────┐
   │  for each runner            │
   │    go runner.Start(runCtx)  │
   └────┬────────────────────────┘
        │
   ◄────┤  wait for:
        │    • ctx.Done()           → external signal (SIGINT/SIGTERM)
        │    • runner exited        → unexpected exit or fatal error
        │
        ▼
  Shutdown(background ctx + timeout)
        │
   ┌────┴────────────────────────┐
   │  for i := len-1 .. 0       │  ← reverse order (LIFO)
   │    go runner.Stop(ctx)      │
   └────┬────────────────────────┘
        │
        ▼
  wg.Wait() → errors.Join(errs...)
```

## Runner implementations

| Runner        | Tick-based  | Purpose                                     |
|---------------|-------------|---------------------------------------------|
| `httpserver`  | no          | Serve HTTP (UI + REST API)                  |
| `grpcserver`  | no          | Serve gRPC (agent discovery)                |
| `lifecycle`   | yes         | Transition stale agents through statuses    |
| `sync`        | yes         | Push pending rollouts to agents via proxy   |

### Server runners (httpserver, grpcserver)
Both follow the same pattern:
1. `New` validates config and handler/server
2. `Start` binds a TCP listener, serves, blocks
3. `Stop` attempts graceful shutdown, falls back to hard close on timeout
4. `ready` channel synchronises Stop with listener binding

### Tick runners (lifecycle, sync)
Both follow the same pattern:
1. `New` validates store dependency
2. `Start` runs a `time.Ticker` loop, calling `tick()` each interval
3. `Stop` closes a signal channel; safe for multiple calls
4. `tick()` lists entities, filters actionable ones, applies transitions

A tick runner is built on `runner/base`, which ticks only while this replica
leads. `base.Runner.OnLeader(fn)` registers a function to run each time the
replica takes leadership, before the first tick of that term. The sync runner and
the history collector use it to tell the history service that another replica
may have written history in the meantime.

### Lifecycle runner
Each tick takes the agents whose next-heartbeat deadline has passed (or was
never set) and moves them along `active → inactive → disconnected → deleted`
by how long they have been silent. `hb` is the heartbeat the agent reported, or
`lifecycle.default_heartbeat` (30s) when it reported none.

| State          | Silence greater than                                     | With hb = 30s |
|----------------|----------------------------------------------------------|---------------|
| `inactive`     | `inactive_multiplier` (2) × hb                           | 60s           |
| `disconnected` | `disconnect_multiplier` (5) × hb                         | 150s          |
| deleted        | max(`delete_after` (24h), `delete_multiplier` (10) × hb) | 24h           |

- **Grace after a restart.** Silence counts from the later of the agent's last
  sync and the runner's start, so an outage is not held against the agents:
  each agent that has not synced yet walks through every state in order and
  gets the full thresholds to sync again first. The grace restarts with every
  start, so a control plane restarted more often than the deletion threshold
  deletes no silent agent.
- **Status only gets worse.** Lifecycle never lowers a status: an agent that was
  already `disconnected` stays so during the grace, and only a sync makes an
  agent `active` again. No event is recorded when the status does not change;
  each change records `agent_inactive`, `agent_disconnected` or `agent_deleted`.
- **Deletion drops the row only.** Deleting an agent drops its row, and with
  it the labels operators set on it; its rollouts and its credential stay.
  Until the agent syncs again, each of its pushes fails with `agent not found`
  until `max_retries`, and its synced rollouts are skipped: no call goes out,
  so its backoff does not change. When it syncs again, its stored token still
  authenticates it, and it is created anew, without labels: the next Deploy
  of a spec that targets it by labels turns its rollout into an uninstall,
  unless the labels are restored first.
- **`delete_after` is a floor.** No agent is deleted sooner, whatever
  `delete_multiplier` says. Discovery refuses a reported heartbeat above 3600s
  (1h), so with the defaults (multiplier 10, `delete_after` 24h) the multiplier
  threshold is at most 10h and never beats the floor. It wins only with a
  `default_heartbeat` above 2.4h (for agents that report none), or a
  `delete_multiplier` above 24 at a 1h heartbeat.

### Sync runner
Each tick reconciles the due rollouts: install, update, uninstall, or an
observation that checks the tasks installed on an agent still exist there.
Failing work backs off by `tick_interval` × 2^(n−1), at most 5m (10s, 20s,
40s, …):

| Backoff | n counts                                       | Until it passes           |
|---------|------------------------------------------------|---------------------------|
| Rollout | failed pushes of the rollout                   | the rollout is not pushed |
| Agent   | unanswered calls since the agent last answered | no call goes to the agent |

- **Rollout backoff.** It comes from the attempts and the last push stored with
  the rollout. After `max_retries` (5) failures the rollout waits for its agent
  to reconnect or a new Deploy.
- **Agent backoff.** A call gets no answer when the push timeout passes or the
  request gets no response; over HTTP a refused or reset connection counts too
  (for gRPC see below). Any answer, an error status included, clears the
  backoff. It lives in memory: a restart clears it. As the backoff starts, the
  runner notes the agent's status and uptime from its row. A later tick that
  finds the agent active again after it was not, or with a lower uptime (it
  restarted), clears the backoff and calls the agent: the reconnect on which
  a sync also resets the agent's failed rollouts. An agent that stays active
  keeps its backoff, at most 5m: it may sync and still be unreachable from
  podium (NAT).
- **One worker per agent.** At most `max_concurrency` (4) agents are worked on
  at once. An agent's rollouts go one after another, each with its own
  `push_timeout` (15s): first the uninstalls of tasks the agent never got
  (they need no call and run even while the agent backs off), then pushes,
  ordered by last failure (rollouts that never failed first, then the oldest
  failure), then the observation, so an observation never holds back a push.
- **A turn gets `push_timeout` of wall time.** Once an agent's turn in a tick
  has used `push_timeout`, it starts no more calls. The call under way
  finishes, so an agent that answers slowly, but within `push_timeout`, holds
  a tick for about one `push_timeout` plus one call, however many rollouts it
  has. What is left waits for the next tick and records nothing.
- **One listing observes all of an agent's rollouts.** The observation is a
  listing of the agent's tasks, with no filter and 1000 to a page, instead of a
  call per rollout. Each page is a call like any other and starts only while
  the turn is within its budget. A synced rollout whose task is missing from
  the listing is queued for a reinstall, but only a complete listing shows a
  task missing. If the budget runs out, a page fails, or the agent returns
  the same cursor twice in a row, no rollout is judged and none is touched; an
  answered failure and a repeated cursor are logged as warnings. The next tick
  lists again from the first page, as a cursor holds only within a tick, so an
  agent that cannot be listed within one turn is not observed. A synced
  rollout without a task makes no call.
- **An unanswered call ends the agent's turn.** A failed push records its
  failure on the rollout; an unanswered listing records nothing and only backs
  the agent off. The agent's other rollouts wait, untouched. An answer,
  whatever its status, does not end the turn. Nor does a refused gRPC
  connection: gRPC reports it as `Unavailable`, which agents also answer with.
  Such calls fail fast and do not back the agent off; each rollout records its
  own failure.
- **Push failures are recorded even after the push timeout.** State writes get
  their own 5s budget instead of what is left of the push timeout, so a push
  that used up its timeout still records the status, the attempt and the
  `sync_failed` event. A tick cancelled by shutdown or lost leadership records
  nothing.
