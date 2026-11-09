# internal/server

Unified lifecycle management for all runtime components of the control plane.

## Package map

```text
server/
├── runner.go       Runner interface (Name / Start / Stop)
├── server.go       Server orchestrator: starts, monitors, shuts down runners
├── config.go       ShutdownDelay and ShutdownTimeout configuration
├── error.go        RunnerError, RunnerExitedError, sentinel errors
│
└── runner/
    ├── base/        leader-gated tick runner and term task, the base of the tick runners
    ├── grpcserver/  gRPC listener → grpc.Server.Serve
    ├── httpserver/  TCP listener  → http.Server.Serve
    ├── ops/         ops listener: /livez /readyz /leaderz /version /metrics, over httpserver
    ├── history/     periodic collection of the agents' terminal runs into the history archive
    ├── lifecycle/   periodic agent liveness checks (active → … → deleted)
    └── sync/        periodic rollout reconciliation (push specs to agents)
```

## Runner interface

```go
type Runner interface {
    Name()  string
    Start(ctx) error   // blocks until done: the listener runners until Stop, the others until Stop or ctx cancelled
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
  Shutdown(background ctx + shutdown_timeout)
        │
   ┌────┴────────────────────────┐
   │  for each runner            │  ← all at once, no order
   │    go runner.Stop(ctx)      │
   └────┬────────────────────────┘
        │
        ▼
  wg.Wait() → errors.Join(errs...)
```

The signal that reaches `Run` is the end of a longer sequence that `app.App.Run`
drives: it turns readiness off and waits `server.shutdown_delay` before it lets
`Run` see the cancellation (see "Shutdown" in `internal/config/README.md`).

## Runner implementations

| Runner | Tick-based | Purpose |
| -- | -- | -- |
| `httpserver` | no | Serve HTTP (UI + REST API, discovery, ops) |
| `grpcserver` | no | Serve gRPC (agent discovery) |
| `lifecycle` | yes | Transition stale agents through statuses |
| `sync` | yes | Push pending rollouts to agents via proxy |
| `history` | yes | Collect the agents' terminal runs |

### Server runners (httpserver, grpcserver)

Both follow the same pattern:

1. `New` validates config and handler/server
1. `Start` binds a TCP listener, serves, and blocks until `Stop`; it ignores its
   context
1. `Stop` shuts down gracefully, falls back to hard close on timeout. For
   `httpserver` that means the listener closes, the requests in flight finish,
   and the ones still running at the deadline are cut (their context is
   cancelled and the connection closed); `Config.OnShutdown` runs as `Stop`
   starts, and is how a stream is told to end. For `grpcserver` it is
   `GracefulStop`, then `Stop`.
1. `ready` channel synchronises Stop with listener binding; `Bound()` is that
   channel, and `Addr()` the bound address, `""` before it is bound or when the
   bind failed.
1. A `Stop` that comes before `Start` marks the runner stopped and closes
   `ready`: `Start` then returns nil at once and never binds, and `Addr()` stays
   `""`. That is the order when a runner exits, or the shutdown signal comes,
   before the goroutine of another runner has reached `Start`: `Run` stops every
   runner at once, and the late one neither binds nor holds `Run` until
   `server.shutdown_timeout` (`TestStopBeforeStartNeverBinds`,
   `TestRunReturnsAtOnceWhenARunnerIsStoppedBeforeItStarts`).

### Metrics

`GET /metrics` on the ops listener (`ops.addr`, see "Ops listener" in
`internal/config/README.md`) serves the Prometheus exposition of
`internal/metrics`: one private registry, never the global default, with the Go
and process collectors. It is unauthenticated like the other ops routes, so it
shares their network boundary, and a node with `ops.addr` empty records the
metrics and serves none. Every name starts with `podium_`. No label carries an
agent, user, spec, task or session ID, or a token
(`TestMetricsLabelsAreBounded`): the `route` of the HTTP metrics is the matched
mux pattern, or `other`, the `method` is a standard method or `other`, and the
other labels take the fixed values below.

| Metric | Type | Labels | Meaning |
| -- | -- | -- | -- |
| `podium_build_info` | gauge | `version`, `commit`, `goversion` | Always 1; `commit` is `unknown` when the binary carries none. |
| `podium_http_requests_total` | counter | `server`, `route`, `method`, `code` | Requests of the `ui` and `discovery` listeners, counted outside every other middleware. |
| `podium_http_request_duration_seconds` | histogram | `server`, `route`, `method` | `prometheus.DefBuckets`. A response with `Content-Type: text/event-stream` is counted above and not timed. |
| `podium_grpc_requests_total` | counter | `method`, `code` | Calls of the gRPC listener, by full method and status code. |
| `podium_grpc_request_duration_seconds` | histogram | `method` | `prometheus.DefBuckets`. Unary calls only: a stream is counted above and not timed. |
| `podium_discovery_syncs_total` | counter | `transport` (`http`, `grpc`), `outcome` | How each sync ended on this node, see below. |
| `podium_logins_total` | counter | `outcome` (`ok`, `failed`, `limited`) | UI login attempts that reached the login service (`access.Service.Login`); `failed` is a wrong subject or password, a user without permissions, and any other error, an empty subject or password included, `limited` an attempt the login limiter refused. |
| `podium_agents` | gauge | `status` (`active`, `inactive`, `disconnected`) | Agents in this node's store, read at scrape. |
| `podium_rollouts` | gauge | `status` (`pending`, `synced`, `drift`, `failed`, `unknown`) | Rollouts in this node's store, read at scrape. |
| `podium_sync_turns_total` | counter | `outcome` (`answered`, `unanswered`, `local`) | Agent turns of the sync runner, which runs on the leader only: `answered` when the agent answered a call, `unanswered` when a call got no answer, `local` when the turn needed no call. A tick that is cancelled reports none. |
| `podium_sync_turn_duration_seconds` | histogram | none | The duration of those turns. |
| `podium_store_commit_duration_seconds` | histogram | `backend` (`durable`, `raft`) | A durable commit (log append, its sync, and the apply), or a raft write from proposal to answer, which only the leader makes. Buckets from 0.5 ms to 1 s. An in-memory store has none. |
| `podium_store_failed` | gauge | none | 1 while the store has failed, which `/readyz` reports as `storage failed`; it reads the same hook, and is 0 on a node whose store cannot fail. |

The outcomes of `podium_discovery_syncs_total`:

| Outcome | A sync that |
| -- | -- |
| `ok` | wrote an agent that podium knew. |
| `enrolled` | wrote an agent that had no row: its first sync. |
| `unauthenticated` | carried a missing or wrong token. |
| `refused` | was a first contact whose token the enrollment secret does not derive. |
| `invalid` | failed validation, the capability caps and the token length included, or whose body could not be read or parsed. |
| `rate_limited` | the sync bucket of its agent, or the limit on token failures, refused (HTTP 429, gRPC `ResourceExhausted`). |
| `unavailable` | this node could not serve: a follower that has no leader to forward to, or cannot reach it, a leader that lost its leadership, or a store that failed. |

A request that is not a sync, such as a GET of the sync path, has no outcome. A
sync that a follower forwards is counted by the leader that serves it, not by
the follower, and a follower counts only the ones it refuses itself
(`TestFollowerCountsRefusedSyncsAsUnavailable`); `enrolled` and `ok` are decided
by a read of the agent's row before the write, so two first syncs of one agent
that race can both count as `enrolled`. The series of
`podium_discovery_syncs_total`, `podium_logins_total` and
`podium_sync_turns_total` exist from the start at 0.

`podium_agents` and `podium_rollouts` are read when the scrape runs, each with
one read of the store that counts the rows where they live (`storage.Counter`),
and copies none: over 5000 agents and 5000 rollouts the two counts took 0.16 ms
and allocated 384 bytes, where scanning the rows took 3.3 ms and 4.5 MB
(`BenchmarkInventoryOnly5000Agents` and `BenchmarkScanInstead5000Agents`, on one
Apple-silicon laptop under Docker), and a whole scrape 0.5 ms
(`BenchmarkScrape5000Agents`). A raft follower reads its own replica, so its
inventory is as stale as its reads (see "Reads on a follower" in
`internal/raft/README.md`). A store that fails to count leaves its gauges out of
that scrape and does not fail it.

The raft node's own metrics are listed in "Metrics" of
`internal/raft/README.md`.

### Tick runners (lifecycle, sync)

Both follow the same pattern:

1. `New` validates store dependency
1. `Start` runs a `time.Ticker` loop, calling `tick()` each interval
1. `Stop` closes a signal channel; safe for multiple calls
1. `tick()` lists entities, filters actionable ones, applies transitions

A tick runner is built on `runner/base`, which ticks only while this replica
leads. `base.Runner.OnLeader(fn)` registers a function to run each time the
replica takes leadership, before the first tick of that term. The sync runner
and the history collector use it to tell the history service that another
replica may have written history in the meantime, and the lifecycle runner to
start the grace of the term (below).

`base.NewTermTask` builds a runner of `runner/base` that never ticks: it runs a
task once at the start of each leadership term. A task that fails while the term
lasts logs one WARN and runs again after a pause, from 1s doubling up to 1m; a
term that ends first, by a leadership loss or a shutdown, leaves the task to the
next term, with one INFO. In raft mode the app seeds through one, the
`bootstrap` runner, see `internal/bootstrap/README.md`.

### Lifecycle runner

Each tick takes the agents whose next-heartbeat deadline has passed (or was
never set) and moves them along `active → inactive → disconnected → deleted` by
how long they have been silent. `hb` is the heartbeat the agent reported, or
`lifecycle.default_heartbeat` (30s) when it reported none.

| State | Silence greater than | With hb = 30s |
| -- | -- | -- |
| `inactive` | `inactive_multiplier` (2) × hb | 60s |
| `disconnected` | `disconnect_multiplier` (5) × hb | 150s |
| deleted | max(`delete_after` (24h), `delete_multiplier` (10) × hb) | 24h |

- **Grace at each leadership term.** Silence counts from the later of the
  agent's last sync and the start of this replica's current leadership term, set
  before the term's first tick, so neither an outage nor the time in which
  another replica led, or none did, is held against the agents: each agent that
  has not synced yet walks through every state in order and gets the full
  thresholds to sync again first. A replica that takes leadership long after it
  started gives the grace all the same. The grace restarts with every term, so a
  control plane restarted, or a cluster whose leader changes, more often than
  the deletion threshold deletes no silent agent. In standalone the one term
  starts when the runner does.
- **Status only gets worse.** Lifecycle never lowers a status: an agent that was
  already `disconnected` stays so during the grace, and only a sync makes an
  agent `active` again. No event is recorded when the status does not change;
  each change records `agent_inactive`, `agent_disconnected` or `agent_deleted`.
- **Deletion drops the row only.** Deleting an agent drops its row, and with it
  the labels operators set on it; its rollouts and its credential stay. Once the
  deletion is committed the runner has the proxy pool close the gRPC connection
  it holds to the agent (`Pool.Forget`); a later call dials afresh. Until the
  agent syncs again, each of its pushes fails with `agent not found` until
  `max_retries`, and its synced rollouts are skipped: no call goes out, so its
  backoff does not change. When it syncs again, its credential still
  authenticates it, and it is created anew, without labels: the next Deploy of a
  spec that targets it by labels turns its rollout into an uninstall, unless the
  labels are restored first.
- **An operator's delete drops the rollouts too.** `DELETE /api/v1/agents/{id}`
  (`agents:delete`, see `internal/handler/README.md`) removes the agent's row,
  if it has one, and every rollout of the agent ID in one transaction, where
  lifecycle deletion leaves the rollouts so that a returning agent resumes them.
  So it also drops what lifecycle deletion leaves: the rollouts of an agent that
  lifecycle deleted, which has no row. It answers 404 only when the ID has
  neither a row nor a rollout. It keeps the credential and the execution
  history, as lifecycle deletion does, closes the pooled connection the same
  way, and records `agent_removed` with the actor, which is no issue, unlike
  lifecycle's `agent_deleted`; it also closes the agent's open `agent_inactive`
  and `agent_disconnected` issues, as a reconnect does. An agent that still runs
  registers again at its next sync with its credential, and comes back with no
  labels and no rollouts (`TestDeletedAgentReturnsWithoutRollouts`). To keep it
  out, reset its credential first (`POST /api/v1/agents/{id}/credential`): its
  old token is then refused, so only the new one brings it back.
- **`delete_after` is a floor.** No agent is deleted sooner, whatever
  `delete_multiplier` says. Discovery refuses a reported heartbeat above 3600s
  (1h), so with the defaults (multiplier 10, `delete_after` 24h) the multiplier
  threshold is at most 10h and never beats the floor. It wins only with a
  `default_heartbeat` above 2.4h (for agents that report none), or a
  `delete_multiplier` above 24 at a 1h heartbeat.

### Sync runner

Each tick reconciles the due rollouts: install, update, uninstall, or an
observation that checks the tasks installed on an agent still exist there.
Failing work backs off by `tick_interval` × 2^(n−1), at most 5m (10s, 20s, 40s,
…):

| Backoff | n counts | Until it passes |
| -- | -- | -- |
| Rollout | failed pushes of the rollout | the rollout is not pushed |
| Agent | unanswered calls since the agent last answered | no call goes to the agent |

- **Rollout backoff.** It comes from the attempts and the last push stored with
  the rollout. After `max_retries` (5) failures the rollout waits for its agent
  to reconnect or a new Deploy.
- **Agent backoff.** A call gets no answer when the push timeout passes or the
  request gets no response; over HTTP a refused or reset connection counts too
  (for gRPC see below). Any answer, an error status included, clears the
  backoff. It lives in memory: a restart clears it. As the backoff starts, the
  runner notes the agent's status and uptime from its row. A later tick that
  finds the agent active again after it was not, or with a lower uptime (it
  restarted), clears the backoff and calls the agent: the reconnect on which a
  sync also resets the agent's failed rollouts. An agent that stays active keeps
  its backoff, at most 5m: it may sync and still be unreachable from podium
  (NAT).
- **One worker per agent.** At most `max_concurrency` (4) agents are worked on
  at once. An agent's rollouts go one after another, each with its own
  `push_timeout` (15s): first the uninstalls of tasks the agent never got (they
  need no call and run even while the agent backs off), then pushes, ordered by
  last failure (rollouts that never failed first, then the oldest failure), then
  the observation, so an observation never holds back a push.
- **A turn gets `push_timeout` of wall time.** Once an agent's turn in a tick
  has used `push_timeout`, it starts no more calls. The call under way finishes,
  so an agent that answers slowly, but within `push_timeout`, holds a tick for
  about one `push_timeout` plus one call, however many rollouts it has. What is
  left waits for the next tick and records nothing.
- **One listing observes all of an agent's rollouts.** The observation is a
  listing of the agent's tasks, with no filter, instead of a call per rollout.
  Each page is a call like any other and starts only while the turn is within
  its budget. A page starts at 1000 tasks. One that the proxy refuses as too
  large (an answer over 4 MiB) or as holding too many elements (more than 262144
  repeated elements in all) is asked again from the same cursor with half the
  limit, down to one task, and the smaller limit holds for the rest of the turn;
  the next turn starts at 1000 again. A task with more than 64 conditions is a
  bound of the task, not of the page: no smaller page avoids it, so the page is
  not asked again and the listing ends at the call that meets it. The proxy
  refuses it at the 65th condition over either transport, so a flood of
  conditions in one status is one call with one cause whichever the agent
  speaks, as long as nothing ahead of it in the answer has spent the budget of
  elements. Over gRPC two floods go another way: a status that the agent splits
  into several occurrences of the task's `status` field, each of 64 conditions
  or fewer, which the scan counts one occurrence at a time, and a flood behind
  tasks that spent the budget. The scan refuses either as too many elements,
  before anything is decoded, once the budget is passed, and the page is asked
  again with half the limit as above: up to ten calls, and ten for a split flood
  that passes the budget even alone, every one refused before decoding. A split
  within the budget is decoded and refused as too many conditions, as below, and
  the listing ends without halving. The listing is bounded, whatever the agent
  answers. The proxy bounds what a page builds (see "What an answer may build"
  in `internal/proxy/README.md`). What is past a bound is not built: the task
  past the limit, the element past 262144 in all (gRPC) and the condition past
  the 64th of a task (both). Over HTTP what is under a bound is built first: the
  first tasks of a page up to the limit, and the first 64 conditions of a task,
  are decoded and dropped with the error; over gRPC the scan refuses such an
  answer before any of it is decoded. A task ID over 1024 bytes and a cursor
  over 4096 bytes are refused once decoded, and so is a gRPC task whose `status`
  field the agent split in several occurrences that hold more than 64 conditions
  in all within the budget, which the decoder merges, at the cost of a page the
  other bounds have capped. The tests measure a refused `ListTasks` flood at 14
  MiB or less allocated in all (what each sends, and what each logged, is in the
  proxy README). The runner counts the tasks of a page again, and refuses a
  cursor over 4096 bytes itself. A listing that would hold more than 65536 task
  IDs or 32 MiB of them, or take more than 1024 calls, refused pages included,
  stops. A synced rollout whose task is missing from the listing is queued for a
  reinstall, but only a complete listing shows a task missing. If the budget
  runs out, a page fails or is refused (a task too large to list, or with too
  many elements, even alone, or with more than 64 conditions, included), the
  listing passes a bound, the agent returns a cursor it returned before, or its
  pages name different snapshots (resource versions that differ; a page without
  one is let through), no rollout is judged and none is touched, and the next
  tick lists again from the first page, as a cursor holds only within a tick. So
  every synced rollout of an agent is observed each tick when, after the turn's
  pushes, the agent's task list can be listed within those bounds, with the last
  page starting inside `push_timeout`; an agent whose list cannot is not
  observed. A synced rollout without a task makes no call.
- **Reinstalls are not bounded by `push_timeout`.** After a complete listing,
  each rollout whose task is missing is queued for a reinstall by a write to the
  store, about 1 ms on the durable store; in raft mode each write is a
  replicated commit. These writes make no agent call, so the turn's budget does
  not stop them: an agent that lost all its tasks has one write per rollout in
  one turn.
- **A listing that cannot finish is logged when that changes.** When an agent's
  listing turns incomplete, the runner logs one warning that names the cause (a
  task too large to list, the budget used, a repeated cursor, pages of different
  snapshots, a page answered with an error, a page with more tasks than asked, a
  task with too many conditions, a task or a page with too many elements even
  alone, a cursor that is too long, a listing over the bounds). When it turns
  complete again, one info line names the latest cause. Neither is repeated on
  every tick, and a cause that changes meanwhile is not logged. An agent that
  flips between the two logs a pair of lines for each flip.
- **An unanswered call ends the agent's turn.** A failed push records its
  failure on the rollout; an unanswered listing records nothing and only backs
  the agent off. The agent's other rollouts wait, untouched. An answer, whatever
  its status, does not end the turn. Nor does a refused gRPC connection: gRPC
  reports it as `Unavailable`, which agents also answer with. Such calls fail
  fast and do not back the agent off; each rollout records its own failure.
- **Push failures are recorded even after the push timeout.** State writes get
  their own 5s budget instead of what is left of the push timeout, so a push
  that used up its timeout still records the status, the attempt and the
  `sync_failed` event. A tick cancelled by shutdown or lost leadership records
  nothing.
- **An agent's error text is cut.** The text of a failed call is the agent's to
  choose. The runner cuts the message it stores in the rollout and records as
  the detail of the `sync_failed` event to 4 KiB on a UTF-8 boundary, with `…`
  where it cut (`proxy.CapAgentText`), and the errors of the proxy that it logs
  read as the cut text. The archive of history, which keeps the error an agent
  reports for a run, does not use the helper: it has a bound of its own, 32 KiB,
  and refuses a record over it.
