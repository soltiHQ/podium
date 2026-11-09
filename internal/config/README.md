# Package config

Package config aggregates per-package configuration structs and provides a
single `Default()` constructor for development use:

- **Config** — top-level struct of every section, each the config struct of its
  owning package under `internal/`: `history` (`service/history`,
  `server/runner/history`), `http` and `http_discovery`
  (`server/runner/httpserver`), `grpc` (`server/runner/grpcserver`), `ops`
  (`server/runner/ops`), `sync` (`server/runner/sync`), `lifecycle`
  (`server/runner/lifecycle`), `triggers` (`uikit/htmx`), `server`, `auth` and
  `cluster` (the packages of those names), `cors` and `streams`
  (`transport/http/middleware`), `events` (`event`), `tls`
  (`transport/tlsconf`), and `agent_auth` and `log`, defined here.
- **Default()** — returns safe development defaults: it takes `cluster`,
  `events` and `history` from their packages' `DefaultConfig()`, and sets the
  listeners' names and addresses, `auth.cookie_secure`, `streams`,
  `agent_auth.require` and `log` itself. A field left at zero takes its
  package's default where the package has one, when the package reads it
  (`withDefaults()` in `server`, `httpserver`, `grpcserver`, `lifecycle` and
  `sync`, `WithDefaults()` in `auth`, `cors`, `event` and `history`); "Every
  setting" lists the value each setting runs with.

## Adding a new sub-config

1. Add a `Config` struct with `withDefaults()` in the owning package.
1. Add the field to `config.Config`.
1. Optionally set non-zero defaults in `Default()`.
1. Pass `cfg.<Field>` in the `internal/app` composition root.

## Loading from external sources

`Load(path, log)` reads in priority order: `Default()` → YAML file (`path`,
which `cmd/main.go` takes from `--config`, or `CONFIG_PATH` when it is empty) →
env, then enforces `Config.Validate()`. Loading is strict: a YAML key that is
misspelled or misplaced, or a mistyped variable under a section's prefix (see
"Environment variables"), stops podium at start, before any listener or store
opens.

### Command line

The command line is parsed strictly, before anything starts and before any
configuration loads (`TestCLIRefusesUnknownFlagBeforeStarting`):

- `podium [--config PATH]` runs the server. `--config` may be spelled `-config`
  or `--config=PATH`.
- `podium agent-token [--generation N] [--] <agent-id>` prints an agent's token
  (see "Enrollment secret").
- `podium version` and `podium --version` print one line,
  `podium <version> (commit <12 hex>[, modified], built <RFC 3339>, <go version> <os>/<arch>)`
  (`TestVersionCommandPrintsOneLine`), and start nothing. The version, the
  commit and the time come from the build information the Go toolchain embeds in
  the binary, and nothing is injected at link time: `dev` stands for a build
  that has no module version, and `unknown` for a commit or a time a build
  without VCS information lacks. The time is the commit's.
- `-h` and `--help` print the usage to stdout and exit 0 (`TestHelpExitsZero`).
- An unknown flag, an unknown subcommand, a flag without its value or an
  argument that belongs to no command prints the error and the usage to stderr
  and exits 2. A subcommand is the first argument:
  `podium --config f.yaml agent-token x` is an error. `podium agent-token`
  answers so with its own usage line (a second agent ID gets the line alone):
  before `--`, any argument that starts with `-`, other than `--generation N`,
  `--generation=N` and a bare `-h` or `--help`, is an unknown option, so an
  agent ID that starts with `-` goes after `--`
  (`TestAgentTokenRefusesUnknownOptions`).

### YAML file

- A key that is not a setting fails loading at any depth. The error names the
  file, the line, the key's full path and the keys known at that level:
  `config: config.yaml:12: unknown key "http.lisen" (known: addr, idle_timeout, ...)`.
- A setting that only the environment may carry (`agent_auth.enroll_secret`,
  `bootstrap_admin_password`) fails the same way when the file holds it, and the
  error names the variable, never the value:
  `agent_auth.enroll_secret is read from SOLTI_AGENT_AUTH_ENROLL_SECRET only; remove it from the file`.
- An empty file, a file of comments only, `---` or `{}` fails with
  `config: <file> holds no settings`.

### Environment variables

The variable of a setting is `SOLTI_` followed by its YAML path in upper case,
the parts joined by `_`: `http.read_header_timeout` is
`SOLTI_HTTP_READ_HEADER_TIMEOUT`, `cluster.raft.tls.ca_file` is
`SOLTI_CLUSTER_RAFT_TLS_CA_FILE`. The two settings that only the environment may
carry keep their own names, `SOLTI_AGENT_AUTH_ENROLL_SECRET` and
`SOLTI_BOOTSTRAP_ADMIN_PASSWORD`. No other name is read, but `CONFIG_PATH`,
which names the file: `HOSTNAME`, `PORT`, `DATA_DIR`, `REQUIRE` and the like,
without `SOLTI_` and the path, change nothing.

A value is read as follows:

- A string is taken as is, spaces included. A set but empty variable sets the
  empty string.
- A bool is `true` or `false` (also `1`, `0`, `t`, `f`, `T`, `F`, `TRUE`,
  `FALSE`, `True`, `False`), as `strconv.ParseBool` reads it.
- An integer is in decimal.
- A duration is a Go duration, such as `30s` or `1m30s`.
- A list is comma-separated, with the spaces around each item trimmed:
  `10.0.0.1, 10.0.0.2` is two items. An empty value is an empty list. An empty
  item (`10.0.0.1,`) stays, for the setting's own check to refuse.
- A set but empty bool, integer or duration is an error, not a default.

A value that does not parse fails loading with the variable's name and what was
expected, never the value, which may be a secret.

A `SOLTI_` variable that is not a setting is handled by where its name falls:

- Under a section's prefix (`SOLTI_HTTP_`, `SOLTI_HTTP_DISCOVERY_`,
  `SOLTI_GRPC_`, `SOLTI_OPS_`, `SOLTI_SYNC_`, `SOLTI_LIFECYCLE_`,
  `SOLTI_TRIGGERS_`, `SOLTI_SERVER_`, `SOLTI_AUTH_`, `SOLTI_CORS_`,
  `SOLTI_CLUSTER_`, `SOLTI_EVENTS_`, `SOLTI_STREAMS_`, `SOLTI_TLS_`,
  `SOLTI_AGENT_AUTH_`, `SOLTI_HISTORY_`, `SOLTI_LOG_`): it is a mistyped
  setting, and loading fails with its name and, when a setting's variable is
  within edit distance 2, that name:
  `unknown environment variable SOLTI_HTTP_ADRR (did you mean SOLTI_HTTP_ADDR?)`.
- Anywhere else: it is logged once at WARN, with the closest setting's variable
  when one is within edit distance 2, and ignored. The solti SDK and its
  examples use `SOLTI_CONTROL_PLANE`, `SOLTI_AGENT_ADVERTISE_URL`,
  `SOLTI_CONTAINERD_*`, `SOLTI_METRICS_ADDR` and `SOLTI_TASK_ENV`, and an agent
  may run in the same shell as podium, so those must not stop it.
  `SOLTI_AGENT_ADVERTISE_URL` is not under `SOLTI_AGENT_AUTH_`, so it only
  warns. A typo in the section's own name, such as `SOLTI_HTPP_ADDR`, is under
  no section's prefix and also only warns, with `SOLTI_HTTP_ADDR` as the
  suggestion.

The top-level setting `trusted_proxies` has no section prefix, so a mistyped
`SOLTI_TRUSTED_PROXY` only warns.

### Validation at load

`Config.Validate()` covers every section. Addresses (`http.addr`,
`http_discovery.addr`, and `grpc.addr` on a TCP network) are non-empty and
`host:port` with a numeric port; `ops.addr` is that too, or empty, which turns
the ops listener off. A negative timeout, interval, size or count fails in
`http`, `http_discovery`, `sync`, `lifecycle`, `server` and
`streams.subscriber_buffer`; zero keeps the setting's default. Each
`triggers.*_refresh` is empty (the default) or `every <n>s`, `every <n>m` or
`every <n>ms`. The other sections keep the checks described under their own
headings. An error names the section (`config.http`, `config.cluster`,
`config.history.archive`, ...).

### Logging

`log.level` is `debug`, `info`, `warn` or `error` (lower case), default `info`:
a line below it is not written. `log.format` is `json` (the default, one JSON
object per line) or `text` (zerolog's console writer, one line per event, no
colour). Both go to stdout. A value that is none of those fails loading as
`config.log` (`TestLoadRejectsAnInvalidLogSetting`).

The settings apply once the configuration is loaded. Before that podium logs
JSON on stdout at `info`: the first line of a run (`podium starting`, with
`version`, `commit`, `modified`, `go_version` and `platform`, and `built` when
the binary carries the commit time), and what loading reports, such as the
warning about a `SOLTI_` variable that is not a setting. After it, every line
podium writes, the raft library's included (see `internal/raft`, "Logging"),
follows `log.level` and `log.format`; a library that logs through its own
default logger, which podium does not route, such as grpc-go's `grpclog` or the
standard `log` that `httputil.ReverseProxy` falls back to, keeps writing to
stderr. A failure to load is logged by the bootstrap logger and exits 1. Errors
that `net/http` reports on its own, such as a failed TLS handshake or the panic
of a handler, are WARN lines with the server's name in `runner`
(`TestHTTPServerErrorsGoToZerolog`).

### Ops listener

`ops.addr` (env `SOLTI_OPS_ADDR`, default `127.0.0.1:8081`) is a listener of its
own for the probes of supervisors and load balancers; an empty value turns it
off. `ops.name` (default `ops`) names its runner in the logs. The listener is
**plain HTTP** and its routes are **never authenticated**: they sit outside the
UI's middleware, set no cookie and show no token, secret or stored record. What
they do tell is the node's health, whether it leads, its build and its metrics,
which on a raft node include its raft state, term, indexes and peer count. Keep
it on an internal network: the default binds loopback only, and a container that
is probed from outside binds `0.0.0.0:8081` and publishes the port to the
orchestrator alone. Every route answers GET and HEAD, as one line of text (`ok`,
or the reason) except `/version` and `/metrics`:

| Route | Answers |
| -- | -- |
| `/livez` | 200 `ok` while the process serves. |
| `/readyz` | 200 `ok` when the node is ready, and otherwise 503 with the reason, one of `starting`, `storage failed`, `no raft leader`, `shutting down`. |
| `/leaderz` | 200 on the raft leader once start-up has finished, whatever the store does and also during the shutdown delay, while the node still serves; on a standalone node, 200 while it is ready. Otherwise 503, with `starting`, `not leader`, or, on a standalone node, the reason it is not ready. |
| `/metrics` | The Prometheus exposition of the node's metrics, in the text format; see "Metrics" in `internal/server/README.md`. |
| `/version` | The build information as JSON: `version`, `modified`, `go_version` and `platform` always, `commit` when the binary carries a VCS revision, `build_time` (the commit time, RFC 3339; the start-up log line calls it `built`) when it carries one. |

A node is ready when start-up has finished (every listener, the ops listener
included, is bound, and the store is open), the store serves, and the shutdown
has not begun. The store serves when a durable standalone store has not failed
(`storage failed` is the state in which its reads and writes fail until restart,
which it also logs as an ERROR) and, on raft, when this node knows a leader.
`/readyz` gives the first reason that applies, in this order: `shutting down`,
`starting`, `storage failed`, `no raft leader` (`TestReadinessReasonOrder`).
`/livez` stays 200 whatever the store does: a restart does not mend a raft
cluster without a leader, and a failed durable store is for the operator to look
at. An in-memory standalone store cannot fail.

The gRPC listener serves `grpc.health.v1.Health` without authentication, though
with `tls.server.client_ca_file` set the listener itself still asks every
client, a health checker included, for a certificate of the agents' CA: the
overall service (`""`) and `solti.discover.v1.DiscoverService` are `SERVING`
while `/readyz` answers 200, and `NOT_SERVING` while it does not. A `Check`
reads the state anew; a client that `Watch`es sees a change within a second, and
its stream ends when the gRPC listener starts to stop.

Tests: `TestReadinessFollowsStoreAndShutdown`, `TestReadinessReasonOrder`,
`TestLeaderProbeOnRaft`, `TestOpsEndpointsNeedNoAuth`,
`TestOpsListenerDisabledWhenAddrEmpty`, `TestGRPCHealthFollowsReadiness`.

### Shutdown

The first SIGINT or SIGTERM starts a graceful shutdown, in this order:

1. `/readyz` turns to 503 `shutting down` and the gRPC health service to
   `NOT_SERVING`, at once. Every listener still serves.
1. podium waits `server.shutdown_delay` (env `SOLTI_SERVER_SHUTDOWN_DELAY`,
   default `0s`, so no wait): the time a load balancer needs to see the 503 and
   stop sending requests. Behind a load balancer or in Kubernetes set `5s`. In
   Kubernetes, a raft deployment that finds its peers through a headless Service
   (`cluster.discovery.driver: dns`) and probes `/readyz` as the
   `readinessProbe` starts only if that Service sets
   `publishNotReadyAddresses: true`: a node is ready only once it knows a
   leader, which needs the other nodes to resolve it, and without that setting
   the Service lists none of them until they are ready.
1. The listeners stop accepting. The requests in flight finish, within
   `server.shutdown_timeout` (default 15s): a request is no longer cut by the
   shutdown. The event stream and the task log streams end as the UI listener
   starts to stop, so a stream that stays open does not hold the others until
   the timeout. A request still running at the timeout is cut. The gRPC listener
   stops with `GracefulStop` within the same timeout; the health `Watch` streams
   end as it starts, so a client that holds one does not delay it
   (`TestHealthWatchDoesNotHoldShutdown`).
1. The rest is as before: the log hub, the proxy pool, the event hub and the
   store or raft node close, bounded by 30s, which is not configurable.

The delay is not part of `server.shutdown_timeout`, so a termination grace
period must cover both, and the 30s of step 4. A **second** SIGINT or SIGTERM,
while the shutdown runs, ends the process at once with status 1, after a WARN
line, and cleans nothing up. A runner that exits on its own, such as a listener
that cannot bind, stops the others without the delay, turns readiness and the
gRPC health service to "shutting down" (`TestRunnerExitFlipsReadiness`), and
exits 1. The turn comes as the UI listener starts to stop, or, when the UI
listener never bound, once every runner has stopped. The ops listener stops with
the others, so after step 2 its probes get a refused connection.

Tests: `TestInFlightRequestFinishesAtShutdown`,
`TestStopLetsRequestsInFlightFinish`, `TestOnShutdownEndsAStream`,
`TestStopCutsARequestThatOutlastsTheDeadline`, `TestSecondSignalExitsAtOnce`.

### Every setting

Default is the value podium runs with when neither file nor environment sets the
setting: the one `Default()` gives, or, where `Default()` leaves the setting at
zero, the default the package or the runner applies to a zero value; for a
setting that is empty here the package or the runner may still apply its own
default or have no value, as the section on it says.

| YAML key | Environment variable | Type | Default |
| -- | -- | -- | -- |
| `history.archive.retention` | `SOLTI_HISTORY_ARCHIVE_RETENTION` | duration | 168h |
| `history.archive.max_records` | `SOLTI_HISTORY_ARCHIVE_MAX_RECORDS` | integer | 10000 |
| `history.archive.max_bytes` | `SOLTI_HISTORY_ARCHIVE_MAX_BYTES` | integer | 33554432 |
| `history.collector.tick_interval` | `SOLTI_HISTORY_COLLECTOR_TICK_INTERVAL` | duration | 5s |
| `history.collector.cycle_timeout` | `SOLTI_HISTORY_COLLECTOR_CYCLE_TIMEOUT` | duration | 8s |
| `history.collector.max_concurrency` | `SOLTI_HISTORY_COLLECTOR_MAX_CONCURRENCY` | integer | 2 |
| `history.collector.max_agents_per_cycle` | `SOLTI_HISTORY_COLLECTOR_MAX_AGENTS_PER_CYCLE` | integer | 100 |
| `history.collector.page_size` | `SOLTI_HISTORY_COLLECTOR_PAGE_SIZE` | integer | 256 |
| `history.collector.max_run_continuations` | `SOLTI_HISTORY_COLLECTOR_MAX_RUN_CONTINUATIONS` | integer | 1024 |
| `history.collector.max_pages` | `SOLTI_HISTORY_COLLECTOR_MAX_PAGES` | integer | 4 |
| `http.read_header_timeout` | `SOLTI_HTTP_READ_HEADER_TIMEOUT` | duration | 10s |
| `http.read_timeout` | `SOLTI_HTTP_READ_TIMEOUT` | duration | 30s |
| `http.write_timeout` | `SOLTI_HTTP_WRITE_TIMEOUT` | duration | 30s |
| `http.idle_timeout` | `SOLTI_HTTP_IDLE_TIMEOUT` | duration | 60s |
| `http.max_header_bytes` | `SOLTI_HTTP_MAX_HEADER_BYTES` | integer | 16384 |
| `http.name` | `SOLTI_HTTP_NAME` | string | http |
| `http.addr` | `SOLTI_HTTP_ADDR` | string | :8080 |
| `http_discovery.read_header_timeout` | `SOLTI_HTTP_DISCOVERY_READ_HEADER_TIMEOUT` | duration | 10s |
| `http_discovery.read_timeout` | `SOLTI_HTTP_DISCOVERY_READ_TIMEOUT` | duration | 30s |
| `http_discovery.write_timeout` | `SOLTI_HTTP_DISCOVERY_WRITE_TIMEOUT` | duration | 30s |
| `http_discovery.idle_timeout` | `SOLTI_HTTP_DISCOVERY_IDLE_TIMEOUT` | duration | 60s |
| `http_discovery.max_header_bytes` | `SOLTI_HTTP_DISCOVERY_MAX_HEADER_BYTES` | integer | 16384 |
| `http_discovery.name` | `SOLTI_HTTP_DISCOVERY_NAME` | string | http-discovery |
| `http_discovery.addr` | `SOLTI_HTTP_DISCOVERY_ADDR` | string | :8082 |
| `grpc.name` | `SOLTI_GRPC_NAME` | string | grpc-discovery |
| `grpc.addr` | `SOLTI_GRPC_ADDR` | string | :50051 |
| `grpc.network` | `SOLTI_GRPC_NETWORK` | string | tcp |
| `ops.name` | `SOLTI_OPS_NAME` | string | ops |
| `ops.addr` | `SOLTI_OPS_ADDR` | string | 127.0.0.1:8081 |
| `sync.tick_interval` | `SOLTI_SYNC_TICK_INTERVAL` | duration | 10s |
| `sync.push_timeout` | `SOLTI_SYNC_PUSH_TIMEOUT` | duration | 15s |
| `sync.max_concurrency` | `SOLTI_SYNC_MAX_CONCURRENCY` | integer | 4 |
| `sync.max_retries` | `SOLTI_SYNC_MAX_RETRIES` | integer | 5 |
| `sync.name` | `SOLTI_SYNC_NAME` | string | sync |
| `lifecycle.tick_interval` | `SOLTI_LIFECYCLE_TICK_INTERVAL` | duration | 10s |
| `lifecycle.default_heartbeat` | `SOLTI_LIFECYCLE_DEFAULT_HEARTBEAT` | duration | 30s |
| `lifecycle.inactive_multiplier` | `SOLTI_LIFECYCLE_INACTIVE_MULTIPLIER` | integer | 2 |
| `lifecycle.disconnect_multiplier` | `SOLTI_LIFECYCLE_DISCONNECT_MULTIPLIER` | integer | 5 |
| `lifecycle.delete_multiplier` | `SOLTI_LIFECYCLE_DELETE_MULTIPLIER` | integer | 10 |
| `lifecycle.delete_after` | `SOLTI_LIFECYCLE_DELETE_AFTER` | duration | 24h |
| `lifecycle.max_concurrency` | `SOLTI_LIFECYCLE_MAX_CONCURRENCY` | integer | 4 |
| `lifecycle.name` | `SOLTI_LIFECYCLE_NAME` | string | lifecycle |
| `triggers.dashboard_refresh` | `SOLTI_TRIGGERS_DASHBOARD_REFRESH` | string | every 60s |
| `triggers.users_refresh` | `SOLTI_TRIGGERS_USERS_REFRESH` | string | every 180s |
| `triggers.user_detail_refresh` | `SOLTI_TRIGGERS_USER_DETAIL_REFRESH` | string | every 300s |
| `triggers.user_sessions_refresh` | `SOLTI_TRIGGERS_USER_SESSIONS_REFRESH` | string | every 180s |
| `triggers.agents_refresh` | `SOLTI_TRIGGERS_AGENTS_REFRESH` | string | every 60s |
| `triggers.agent_detail_refresh` | `SOLTI_TRIGGERS_AGENT_DETAIL_REFRESH` | string | every 60s |
| `triggers.agent_tasks_refresh` | `SOLTI_TRIGGERS_AGENT_TASKS_REFRESH` | string | every 60s |
| `triggers.specs_refresh` | `SOLTI_TRIGGERS_SPECS_REFRESH` | string | every 180s |
| `triggers.spec_detail_refresh` | `SOLTI_TRIGGERS_SPEC_DETAIL_REFRESH` | string | every 60s |
| `server.shutdown_delay` | `SOLTI_SERVER_SHUTDOWN_DELAY` | duration | 0s |
| `server.shutdown_timeout` | `SOLTI_SERVER_SHUTDOWN_TIMEOUT` | duration | 15s |
| `auth.jwt_secret` | `SOLTI_AUTH_JWT_SECRET` | string | empty |
| `auth.audience` | `SOLTI_AUTH_AUDIENCE` | string | control-plane |
| `auth.issuer` | `SOLTI_AUTH_ISSUER` | string | solti |
| `auth.access_ttl` | `SOLTI_AUTH_ACCESS_TTL` | duration | 15m |
| `auth.refresh_ttl` | `SOLTI_AUTH_REFRESH_TTL` | duration | 168h |
| `auth.rate_window` | `SOLTI_AUTH_RATE_WINDOW` | duration | 1m |
| `auth.rate_attempts` | `SOLTI_AUTH_RATE_ATTEMPTS` | integer | 5 |
| `auth.rotate_refresh` | `SOLTI_AUTH_ROTATE_REFRESH` | bool | false |
| `auth.cookie_secure` | `SOLTI_AUTH_COOKIE_SECURE` | string | auto |
| `cors.allow_origins` | `SOLTI_CORS_ALLOW_ORIGINS` | list | empty |
| `cors.allow_methods` | `SOLTI_CORS_ALLOW_METHODS` | list | GET, POST, PUT, DELETE, PATCH, OPTIONS |
| `cors.allow_headers` | `SOLTI_CORS_ALLOW_HEADERS` | list | Authorization, Content-Type, X-Request-Id, If-Match |
| `cors.expose_headers` | `SOLTI_CORS_EXPOSE_HEADERS` | list | X-Request-Id, ETag |
| `cors.allow_credentials` | `SOLTI_CORS_ALLOW_CREDENTIALS` | bool | false |
| `cors.max_age` | `SOLTI_CORS_MAX_AGE` | duration | 12h |
| `cluster.standalone.ephemeral` | `SOLTI_CLUSTER_STANDALONE_EPHEMERAL` | bool | false |
| `cluster.standalone.data_dir` | `SOLTI_CLUSTER_STANDALONE_DATA_DIR` | string | data/podium |
| `cluster.standalone.snapshot_threshold` | `SOLTI_CLUSTER_STANDALONE_SNAPSHOT_THRESHOLD` | integer | 4096 |
| `cluster.backend` | `SOLTI_CLUSTER_BACKEND` | string | standalone |
| `cluster.raft.node_id` | `SOLTI_CLUSTER_RAFT_NODE_ID` | string | empty |
| `cluster.raft.bind_addr` | `SOLTI_CLUSTER_RAFT_BIND_ADDR` | string | empty |
| `cluster.raft.advertise_addr` | `SOLTI_CLUSTER_RAFT_ADVERTISE_ADDR` | string | empty |
| `cluster.raft.data_dir` | `SOLTI_CLUSTER_RAFT_DATA_DIR` | string | empty |
| `cluster.raft.join` | `SOLTI_CLUSTER_RAFT_JOIN` | bool | false |
| `cluster.raft.election_timeout_ms` | `SOLTI_CLUSTER_RAFT_ELECTION_TIMEOUT_MS` | integer | 1000 |
| `cluster.raft.heartbeat_timeout_ms` | `SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS` | integer | 1000 |
| `cluster.raft.snapshot_interval` | `SOLTI_CLUSTER_RAFT_SNAPSHOT_INTERVAL` | duration | 2m |
| `cluster.raft.snapshot_threshold` | `SOLTI_CLUSTER_RAFT_SNAPSHOT_THRESHOLD` | integer | 8192 |
| `cluster.raft.trailing_logs` | `SOLTI_CLUSTER_RAFT_TRAILING_LOGS` | integer | 10240 |
| `cluster.raft.tls.cert_file` | `SOLTI_CLUSTER_RAFT_TLS_CERT_FILE` | string | empty |
| `cluster.raft.tls.key_file` | `SOLTI_CLUSTER_RAFT_TLS_KEY_FILE` | string | empty |
| `cluster.raft.tls.ca_file` | `SOLTI_CLUSTER_RAFT_TLS_CA_FILE` | string | empty |
| `cluster.raft.allow_insecure_transport` | `SOLTI_CLUSTER_RAFT_ALLOW_INSECURE_TRANSPORT` | bool | false |
| `cluster.discovery.driver` | `SOLTI_CLUSTER_DISCOVERY_DRIVER` | string | empty |
| `cluster.discovery.peers` | `SOLTI_CLUSTER_DISCOVERY_PEERS` | list | empty |
| `cluster.discovery.hostname` | `SOLTI_CLUSTER_DISCOVERY_HOSTNAME` | string | empty |
| `cluster.discovery.port` | `SOLTI_CLUSTER_DISCOVERY_PORT` | integer | empty |
| `events.coalesce` | `SOLTI_EVENTS_COALESCE` | duration | 1s |
| `streams.max_per_ip` | `SOLTI_STREAMS_MAX_PER_IP` | integer | 10 |
| `streams.subscriber_buffer` | `SOLTI_STREAMS_SUBSCRIBER_BUFFER` | integer | 64 |
| `tls.server.cert_file` | `SOLTI_TLS_SERVER_CERT_FILE` | string | empty |
| `tls.server.key_file` | `SOLTI_TLS_SERVER_KEY_FILE` | string | empty |
| `tls.server.client_ca_file` | `SOLTI_TLS_SERVER_CLIENT_CA_FILE` | string | empty |
| `tls.client.ca_file` | `SOLTI_TLS_CLIENT_CA_FILE` | string | empty |
| `tls.client.cert_file` | `SOLTI_TLS_CLIENT_CERT_FILE` | string | empty |
| `tls.client.key_file` | `SOLTI_TLS_CLIENT_KEY_FILE` | string | empty |
| `agent_auth.require` | `SOLTI_AGENT_AUTH_REQUIRE` | bool | true |
| not in a file | `SOLTI_AGENT_AUTH_ENROLL_SECRET` | string | no default |
| `log.level` | `SOLTI_LOG_LEVEL` | string | info |
| `log.format` | `SOLTI_LOG_FORMAT` | string | json |
| `trusted_proxies` | `SOLTI_TRUSTED_PROXIES` | list | empty |
| not in a file | `SOLTI_BOOTSTRAP_ADMIN_PASSWORD` | string | no default |

## TLS

`config.TLS` is one shared block applied to the HTTP, HTTP-discovery and gRPC
listeners and to podium's outbound calls (mirrors the SDK's `solti-tls`); the
ops listener is always plain HTTP, and the raft transport has TLS settings of
its own. Both halves are opt-in: an empty `tls.server` serves plaintext, and
with an empty `tls.client` podium dials an agent's gRPC or `http://` endpoint in
plaintext, but an `https://` endpoint, or a leader that serves TLS, over TLS
against the system roots.

- `tls.server` (`cert_file`, `key_file`, `client_ca_file`) — the CP's serving
  identity for the HTTP, HTTP-discovery and gRPC listeners. Setting
  `client_ca_file` turns on **mTLS** (client cert required) on the
  HTTP-discovery and gRPC listeners; the HTTP listener of the UI and the API
  never asks for a client certificate, so browsers keep working.
- `tls.client` (`ca_file`, `cert_file`, `key_file`) — the CP as a client when
  dialing agents (proxy). `ca_file` verifies agent certs; `cert_file`/`key_file`
  present a client cert for mTLS.

```yaml
tls:
  server:
    cert_file: /etc/solti/tls/server.crt
    key_file:  /etc/solti/tls/server.key
    client_ca_file: /etc/solti/tls/agents-ca.crt   # optional → mTLS
  client:
    ca_file:   /etc/solti/tls/agents-ca.crt
    cert_file: /etc/solti/tls/cp-client.crt        # optional → mTLS
    key_file:  /etc/solti/tls/cp-client.key
```

Env equivalents: `SOLTI_TLS_SERVER_CERT_FILE`, `SOLTI_TLS_CLIENT_CA_FILE`, etc.

In raft mode a follower forwards the writes it gets on the HTTP and
HTTP-discovery listeners, and the syncs it gets on the gRPC listener, to the
leader, at the host of the leader's raft advertise address, as written, and this
node's port of that listener (see "Cluster"). With `tls.server` set it forwards
over TLS with `tls.client`: `ca_file` must also hold the CA of the replicas'
server certificates, which must name each replica's raft advertise host as
written, an IP SAN for `10.0.0.1:7000` and a DNS SAN for `podium-0.podium:7000`,
and `cert_file`/`key_file` are presented when the leader asks for a client
certificate, as the discovery listener does with `client_ca_file`. Without a
`tls.client.ca_file`, the leader is verified against the system roots. A leader
whose certificate does not verify gets no write: the follower answers 502. The
raft transport has TLS settings of its own, with a CA of its own, see "Raft
transport security".

The forwards carry logins, cookies, bearer tokens and agents' tokens, so a raft
cluster needs `tls.server` on every node: without it a follower forwards them in
plaintext, and each raft node logs one WARN at start,
`raft forwarding runs plaintext`. With agent mTLS (`client_ca_file`), the
leader's discovery listeners want a client certificate of the agents' CA from a
follower that forwards a sync too, so each node's `tls.client.cert_file` and
`key_file` must chain to the agents' CA: without `tls.client.cert_file` every
sync that reaches a follower fails, 502 over HTTP and `leader unreachable` over
gRPC, and each raft node logs one WARN at start that says so. A follower dials
the leader directly, never through the proxy that `HTTP_PROXY` or `HTTPS_PROXY`
names. Standalone forwards nothing, and logs neither WARN.

## Agent authentication

`agent_auth.require` defaults to `true`. It authenticates enrolled agents: a
sync for an agent ID that has a token, but without that token, gets HTTP 401 /
gRPC `Unauthenticated`, and outbound calls to the agent present that token: the
one podium stores for it, or, for an agent enrolled with the enrollment secret,
the one podium derives from the secret again (see "Enrollment secret" below). By
itself it does not keep other hosts out: the first sync of an unknown agent ID
enrolls the token it presents (trust on first use) and sets the endpoint podium
will call. Close enrollment with an enrollment secret (see "Enrollment secret"
below) or with client certificates (`tls.server.client_ca_file`), or keep the
discovery listeners (`http_discovery.addr`, default `:8082`, and `grpc.addr`,
default `:50051`) reachable only from agent networks. With neither a secret nor
a client CA, startup logs one WARN line, with the two listen addresses:
`agent authentication covers enrolled agents only: the first sync of an unknown agent ID enrolls it and sets the endpoint podium calls, so set SOLTI_AGENT_AUTH_ENROLL_SECRET or tls.server.client_ca_file, or keep the discovery listeners reachable only from agent networks`.

With `require: true`, a token longer than 1024 bytes is refused before it is
compared or enrolled, after the payload and the sync rate are checked, with HTTP
400 `Agent token is too long.` / gRPC `InvalidArgument`, and nothing is
enrolled. The cap applies to what agents send, never to stored state: a
credential an older version stored with a longer token still loads, so podium
starts, but that agent's syncs are refused with 400, until its credential is
reset (see "Resetting a credential" below). gRPC discovery caps request headers
at 16 KiB, like the HTTP listeners.

The capability fields of a sync have limits, checked with the rest of the
payload before any token is checked or enrolled: at most 64 runners, each name
at most 128 bytes; and per runner at most 256 labels (key at most 317 bytes,
value at most 1024 bytes) and at most 256 workloads (`api_version` at most 317
bytes, `kind` at most 128 bytes). A sync that breaks one is refused with HTTP
400 / gRPC `InvalidArgument`, naming the field, as in
`agent capabilities.runners[1].labels has a value over 1024 bytes`, and nothing
is enrolled or stored. The SDK treats a 400 as permanent and does not retry it,
so an agent that breaks one stays refused until it sends data within the limits.
No byte limit that refuses is below the SDK's own: there a label key is a
qualified name, an optional DNS prefix of at most 253 bytes, a slash and a name
of at most 63, and an `api_version` is a CRD group of at most 253 bytes, a slash
and a version of at most 63, so both are 317 bytes at most, and its runner
names, label values and kinds are 63 bytes at most. The counts are below what
the SDK lets an agent send, since it sets no limit on how many runners, labels
or workloads there are: an agent with more than 64 runners, or more than 256
labels or 256 workloads in one runner, gets that 400. `os`, `arch` and
`platform` are never refused for their length: each is cut to 256 bytes, on a
rune boundary, and the sync lands (on Linux the SDK sends the `PRETTY_NAME` of
the host's `/etc/os-release` as `os`, and does not bound it). The limits apply
to what agents send, not to rows already stored. Whatever the limits, a sync is
still bounded by the 256 KiB of its body.

SDK agents send their token only over TLS unless the agent explicitly allows
insecure token transport (`allow_insecure_token_transport()`). With
`require: true`, a sync without a token is refused like a wrong one and logged
at WARN with the agent ID and the three ways out: TLS, so the SDK sends its
token; the SDK's insecure-token option; or `agent_auth.require: false`. With an
enrollment secret set, the line names `podium agent-token` instead of
`agent_auth.require: false`, which fails validation next to a secret. The
discovery limiter bounds these lines to `auth.rate_attempts` per
`auth.rate_window` for each client IP and agent ID.

```yaml
agent_auth:
  require: false   # env SOLTI_AGENT_AUTH_REQUIRE=false
```

`require: false` turns off both directions: no token is checked and none is
sent. Startup then logs one WARN line:
`agent authentication disabled: any host that reaches the discovery listeners can register agents`.
The sync rate (see "Rate limiting") stays on.

### Enrollment secret

`SOLTI_AGENT_AUTH_ENROLL_SECRET` closes enrollment. It is env-only: the field
has no YAML key, so a config file cannot carry it. It needs 32 bytes at least
and is used as is, without trimming; an empty value counts as unset. A shorter
one fails configuration loading with an error that names the variable and not
the value. So does setting it while `agent_auth.require` is `false`, where it
would close nothing.

With the secret set, the first sync of an unknown agent ID enrolls only if its
bearer token is the token of generation 0 derived for the ID: the lowercase hex
of HMAC-SHA256 of the agent ID under the secret (64 characters). Any other
token, a missing one included, is refused like a wrong one: HTTP 401 / gRPC
`Unauthenticated`, nothing is enrolled, and it counts as a failure of the
discovery limiter (see "Rate limiting"). A wrong token is also logged at WARN
with the agent ID and the client IP, never the token, bounded by the discovery
limiter like the WARN for a missing token. A token over 1024 bytes still gets
the 400 above. In raft mode each replica reads its own environment: give every
replica the same secret.

The secret is the root of every credential enrolled under it. Such an agent gets
a derived credential, which stores no token: only its generation and an ID of
the secret, the first 8 bytes of HMAC-SHA256 of `podium:agent-secret-id:v1`
under the secret. That label holds a colon, which no agent ID holds, and no NUL
byte, which the message of every token after generation 0 holds, so the ID is
part of no agent's token. It tells two secrets apart and no token follows from
it, though, like any token, it would confirm a guess of a weak secret offline.
Podium derives the token again from the secret, the agent ID and the generation
to check each sync and to call the agent, so with the secret set no file in the
data directory holds the token of an agent enrolled under it, after a snapshot
too (`TestDerivedCredentialStoresNoToken`, in `internal/storage/durable` and
`internal/raft`). An agent that enrolls without the secret gets a stored
credential, which holds the token it chose, and every credential an older
version enrolled is a stored one too, as it stored every token, one derived from
a secret included. A stored token is compared as it always was, whether a secret
is set or not. A stored token that is the generation-0 token of its agent ID
under the current secret, as `podium agent-token` printed it for agents
provisioned before this version, becomes a derived credential: at the agent's
next sync, in the transaction that checks it, and at each start of standalone
and each leadership term of a raft node, for every such credential
(`TestLegacyRowsConvertedAtStart`, in `internal/app`, shows a standalone start).
A stored token that an earlier secret derived stays stored: it keeps working,
and whoever holds that earlier secret can derive it. Each such start and term
then logs, when there are any, at WARN with `stored_credentials`, how many
stored credentials are left,
`agent credentials: stored tokens are left, kept raw in podium's state and in its backups: tokens agents chose while no enrollment secret was set, and tokens an earlier secret derived, which stay valid and which that secret still derives; resetting an agent's credential (POST /api/v1/agents/{id}/credential) while an enrollment secret is set moves it to a derived credential`,
and, at ERROR with `unusable_credentials`, how many derived credentials it
cannot use, because no secret is set or they derive from another one; a count of
none logs nothing. With `agent_auth.require: false` podium neither converts nor
counts anything.

Upgrading leaves the raw tokens already written where they are: in the backups
taken before the upgrade; in the snapshot and the log entries written before it,
until the store compacts them; and in the freed pages of the database files that
held them, until bbolt writes over those. Standalone compacts at its next
snapshot (see "Standalone storage"). A raft node drops log entries at a snapshot
but keeps the last `cluster.raft.trailing_logs` of them, and keeps its two
latest snapshots (see "Log compaction" in `internal/raft/README.md`). Take a
fresh backup once the store has compacted past the upgrade, and drop the older
ones; a copy of a database file copies its freed pages too, which may still hold
some of those bytes.

Changing or removing the secret locks out every agent whose credential derives
from it: each sync it makes with the token it has is refused like a wrong token,
and logged at WARN, bounded by the discovery limiter, with its agent ID, its
client IP and
`agent <id> holds a credential derived from another enrollment secret than the current one: give it the token that podium agent-token --generation <g> <id> prints with the current secret, set the secret it derives from again, or reset its credential (POST /api/v1/agents/<id>/credential), which issues a token derived under the current secret`
(`agent <id> holds a credential derived from an enrollment secret, and no enrollment secret is set: set that secret again, or reset its credential (POST /api/v1/agents/<id>/credential), which issues a stored token`
without a secret). Podium makes no call to it either: its token lookup fails
with the same text, which a rollout to it records in its error
(`TestChangedSecretRefusesDerivedAgentClearly`, in `internal/handler`;
`TestOutboundUsesDerivedToken`, in `internal/server/runner/sync`). Agents with a
stored token are not affected: their token keeps working, so a token a former
secret derived, which an older version stored, can still be derived by whoever
holds that former secret. To re-provision the fleet under a new secret:

1. Set the new secret on every node and restart them; in raft mode every node
   needs the same secret (see "Settings every replica must share").

1. For each agent the WARN names, derive its token under the new secret with the
   generation the WARN gives, and give the token to the agent. The generation is
   0 for an agent that was never reset; for one that was, it is a ten-digit
   number, about the time of its last reset in Unix seconds:

   ```sh
   export SOLTI_AGENT_AUTH_ENROLL_SECRET=...   # the new secret
   podium agent-token --generation 0 agent-1   # the generation the WARN names
   ```

1. The agent's next sync with that token moves its credential to the new secret,
   at the same generation, and podium calls it again
   (`TestReprovisionedAgentMovesToTheCurrentSecret`, in
   `internal/service/agent`).

Resetting the agent's credential (see "Resetting a credential" below) is the
other way: it moves a credential of another secret to the current one at a later
generation, and the answer holds the token to give the agent, with no host that
runs `podium agent-token` needed
(`TestResetCredentialIssuesTheLaterOfTheNextGenerationAndTheTime`, in
`internal/service/agent`).

Removing the secret leaves those agents these ways in: setting the same secret
again, since without one podium derives no token; setting another secret, and
re-provisioning or resetting each one under it, as above; or resetting each
one's credential while no secret is set, which issues it a stored random token
that lets it back in (see "Resetting a credential" below;
`TestResetCredentialWithoutSecretStoresARandomToken`, in
`internal/service/agent`).

To provision an agent, derive its token on a host that has the secret, then give
the token to the agent as its bearer token. The agent must run with the same ID:

```sh
export SOLTI_AGENT_AUTH_ENROLL_SECRET=...   # the value podium runs with
podium agent-token agent-1                   # prints the token and a newline
```

In the agent, pass it to the SDK with `with_token(Token::new(token)?)`, or load
it with `Token::from_env(...)`. Podium then presents the same token on its calls
to that agent. The SDK still sends it over TLS only, unless the agent opts in
with `allow_insecure_token_transport()`. Agents never see the secret, only their
own token: a leaked token lets nobody enroll another ID.

An agent with a stored token, which it chose or which an older version stored,
is checked against that token, so re-provisioning it with a token derived from
the current secret locks it out: every sync it makes gets 401. That includes an
agent that lifecycle deleted: deletion drops the agent row but keeps its
credential, so the ID stays enrolled. Instead, keep the agent's existing token,
or reset its credential (see "Resetting a credential" below), which with the
secret set replaces the stored token by a derived credential and gives the token
to hand to the agent. For an agent enrolled under the current secret,
`podium agent-token` with the agent's generation prints the token it already
has.

`podium agent-token [--generation N] <agent-id>` must come first among the
arguments. It prints the token of the ID at generation `N`, 0 by default, the
token the ID enrolls with; `--generation=N` works too, with `N` a whole number
from 0 to 4294967295, and comes before the ID. It checks the ID like discovery
does (1 to 128 bytes of `[A-Za-z0-9._-]`) and reads the secret from the
environment only. It never prints the secret, and exits non-zero with one short
line when the secret is missing or shorter than 32 bytes, the ID is invalid, or
`--generation` has no value or one that is no such number. It needs no
configuration file. A bare `-h` or `--help` prints the usage and exits 0. Any
other argument before `--` that starts with `-`, other than `--generation N` and
`--generation=N`, is an unknown option: one line on stderr that names it, and
exit 2, with nothing printed to stdout. An agent ID that starts with `-` goes
after `--`: `podium agent-token -- -h`.

### Resetting a credential

`POST /api/v1/agents/{id}/credential`, which needs `agents:reset` (see
`internal/handler/README.md`), issues the agent a new credential in place of the
one it holds, and ends the old one. It replaces a token that leaked or was lost,
and moves a stored credential to a derived one.

- With the enrollment secret set, the new credential is derived, and it names
  the current secret, whatever secret the old credential derived from. Its
  generation is the later of the old generation plus one, a stored credential
  having none and counting as 0, and the time of the reset in Unix seconds, at
  least 1 and below the last generation: the generation of an agent that was
  reset is a ten-digit number, about the time in seconds
  (`TestResetCredentialIssuesTheLaterOfTheNextGenerationAndTheTime` and
  `TestResetTakesTheTimeFloor`, in `internal/service/agent`). The time is there
  because the old generation can be below one that was issued and revoked: a
  stored credential counts as 0 though its agent held derived ones before (a
  secret removed, a reset while none was set, the same secret set again), and a
  restored backup brings a credential back below the generations reset since
  (`TestResetAfterAStoredInterludeRevivesNoGeneration` and
  `TestResetAfterARestoredBackupExceedsEveryGenerationIssued`, in the same
  package). The generation is above every earlier one as long as each reset of
  the agent falls in a later second than the one before it, on the clock of the
  leader, the node that serves the reset; a reset in the same second as the last
  takes the generation after it. The answer is
  `{"agent_id", "token", "generation"}`, and
  `podium agent-token --generation <generation> <agent-id>`, run with the
  secret, prints the token again. A stored credential reset this way is no
  longer stored: the next start, or leadership term, counts one fewer in the
  WARN above.
- Without a secret, the new credential is stored, with a new random token of 32
  bytes as 64 hex digits (`TestResetWithoutSecretIssuesRandomToken`, in
  `internal/handler`). The answer has no `generation`, and nothing derives that
  token again.
- The old token stops working at the commit, in both directions: a sync with it
  gets 401 / `Unauthenticated`, and podium's calls to the agent carry the new
  token (`TestResetCredentialRevokesBothDirections`). The agent has to be given
  the new token: until it is, podium refuses its syncs, and the calls podium
  makes to it carry a token that it does not hold. The node that served the
  reset also closes its pooled gRPC connection to the agent.
- No response, event or log other than the answer holds the token: not the event
  the reset records (`agent_credential_reset`, with the actor and the agent ID),
  no log line, and no later read. A reset without a secret also keeps its random
  token in podium's state, as every stored token is kept; a derived credential
  holds no token.
- With `agent_auth.require: false` no token is checked or presented, so there is
  no credential to reset: the answer is 409 and nothing changes. An ID with no
  credential gets 404; it needs no agent row. A derived credential that has
  reached generation 4294967295 gets 409, since the next generation would wrap
  around to 0, the token its agent ID enrolls with.
- In raft mode the reset is a leader write, which a follower forwards like any
  write (`TestResetThroughFollowerForwards`, in `internal/app`).

`DELETE /api/v1/agents/{id}` (`agents:delete`) removes the agent's row, if it
has one, and every rollout of the ID, so it also drops the rollouts that
lifecycle deletion leaves, and keeps its credential, as lifecycle deletion keeps
it: an agent that still runs registers again at its next sync with that
credential. To keep it out, reset its credential first, so that its old token is
refused, and delete it after (see "Lifecycle runner" in
`internal/server/README.md`).

## Client IPs and trusted proxies

`trusted_proxies` lists the reverse proxies whose forwarding headers podium
believes. It defaults to empty: the TCP peer is the client.

```yaml
trusted_proxies:   # env SOLTI_TRUSTED_PROXIES=10.0.0.1,10.1.0.0/16
  - 10.0.0.1
  - 10.1.0.0/16
```

Entries are IP addresses or CIDR prefixes. The environment list is trimmed of
spaces around the commas, so `SOLTI_TRUSTED_PROXIES="10.0.0.1, 10.0.0.2"` works;
anything else, an empty entry included, fails configuration loading. The headers
count only when the TCP peer is in the list:

- `X-Forwarded-For`: all header lines are joined in order and walked from the
  right. Trusted entries are skipped; the first untrusted entry is the client.
  When every entry is trusted, the left-most one is. A missing or empty header,
  or an entry the walk cannot parse, leaves the peer as the client. Entries may
  be `ip`, `ip:port` or `[ip]:port`.
- `X-Forwarded-Proto`: the first value of the first header line, compared with
  `https` case-insensitively, says whether the client used HTTPS.

RFC 7239 `Forwarded` is not supported: behind a proxy that sends only
`Forwarded`, the proxy is the client. List only proxies that append the peer to
`X-Forwarded-For` (or replace the header); a listed host that passes a client's
header through unchanged lets that client pick its own IP.

In raft mode a follower forwards writes to the leader, which then sees the
follower as the peer, so every node also trusts, besides this list, the hosts of
its current raft configuration: there is no need to list the replicas. A host
written as an IP counts as soon as the configuration holds it, and stops
counting as soon as it leaves; a host written as a name counts at the addresses
it resolves to, looked up every 10 s, so its trust is only as good as the DNS
answers the node gets; where DNS cannot be trusted that far, write the advertise
addresses as IPs. Each node must connect to the others from the address its raft
address names, or resolves to. Any process on a node's host can then pick the
client IP of its requests, as behind any listed proxy, so a raft host must not
relay outside clients at L4 (a port forward, SNAT, or kube-proxy for a
hostNetwork pod), just as a listed proxy must not pass a client's header
through: such a client would reach the leader from the host's address and pick
its own IP. See "Forwarding and trusted hosts" in `internal/raft/README.md`.

gRPC discovery reads the client IP from the `x-forwarded-for` metadata of a
trusted host only in raft mode, where a follower sets it when it forwards a
sync. In standalone it ignores this setting and always uses the peer address.

## Browser sessions

`auth.cookie_secure` (env `SOLTI_AUTH_COOKIE_SECURE`) decides when the three
auth cookies carry the `Secure` attribute, on every response that sets or clears
them (login, logout, refresh, a revoked session), and so does the device cookie
that login sets (see "Rate limiting"):

- `auto` (default): when the client used HTTPS, either a TLS connection or
  `X-Forwarded-Proto: https` from a proxy in `trusted_proxies`, or, in raft
  mode, from a host of the raft configuration, which a follower that forwards a
  request is (see "Client IPs and trusted proxies").
- `always`: on every response. Use it when the proxy that terminates TLS cannot
  be listed in `trusted_proxies`.

Any other value, an empty one included, fails configuration loading.

`auth.rotate_refresh` (env `SOLTI_AUTH_ROTATE_REFRESH`, default `false`) makes
every refresh of a browser's session replace its refresh token. The replacement
is a write, so in raft mode only the leader can make it. A browser whose access
token has expired (`auth.access_ttl`, 15m by default) and whose request, a GET
included, reaches a follower gets 503 instead of a new token, with `X-Leader`
when the follower knows its leader (see "Leader hint" in
`internal/transport/README.md`), and sets no cookie; the follower logs a WARN,
`auth refresh needs the leader`, with the method and the path, and the access
log of an API route records a 503 with `X-Leader` as a WARN too, so the refresh
leaves no ERROR line while the follower knows its leader. A refresh on a leader
that loses its leadership as the rotation commits gets 503 too, with `X-Leader`
when the node knows the new leader by then, and its WARN,
`auth refresh outcome unknown`, says that the rotation may have taken effect: a
retry at `X-Leader` refreshes the browser when it did not, and when it did, the
browser logs in again. A refresh on a node that shuts down as the rotation
commits has the same outcome and WARN, but its 503 carries no `X-Leader`, as the
node knows no leader any more, so the access log of an API route records it as
an ERROR, and a retry has to go to another node. So with rotation on, a raft
deployment needs leader-sticky routing. The ops listener's `/leaderz` (see "Ops
listener") answers 200 on the leader only and needs no credentials, so a
balancer can route to the leader with it: keep rotation off in raft mode unless
such routing is in place. See "Browsers and `auth.rotate_refresh`" in the
runbook of `internal/raft/README.md`. Without rotation a refresh only reads, and
works on every node, as does a request whose access token is still valid.
Standalone is unaffected.

In raft mode every node checks an access token, and refreshes a session without
rotation, against its own store, which follows the leader's with a lag: a
session revoked, or a user disabled, on the leader still works on a follower
until the follower applies the change, and on a node cut off from the leader
until it reaches it again. See "Reads on a follower" in
`internal/raft/README.md`.

The main listener refuses state-changing requests that a browser sends from
another origin (Go's `http.CrossOriginProtection`) with 403
`Cross-origin request blocked.`: a request whose `Sec-Fetch-Site` is not
`same-origin` or `none`, or, from a browser that does not send that header, one
whose `Origin` does not match `Host`. GET, HEAD and OPTIONS always pass, as do
clients that send neither header (scripts, agents). Behind a proxy that rewrites
`Host`, such old browsers are refused. The check runs before any route, so a
refusal gets no request ID or access-log line; it is logged at WARN with the
method, path, `Sec-Fetch-Site` and `Origin`. The discovery listener has no such
check.

`cors.allow_origins` is empty by default: no other origin gets CORS access.

```yaml
cors:
  allow_origins:
    - https://ops.example.com
  allow_credentials: true
```

Each entry is an exact `scheme://host[:port]`, without a path; anything else
fails configuration loading. A listed origin may also send mutations past the
cross-origin check. `"*"` lets any origin read through CORS, but trusts none
with a mutation, and cannot be combined with `allow_credentials`. By default a
preflight allows the request headers `Authorization`, `Content-Type`,
`X-Request-Id` and `If-Match`, and responses expose `X-Request-Id` and `ETag`,
so a client of another origin can read a placement's `ETag` and send it back as
`If-Match` on Deploy. `allow_headers` and `expose_headers` replace these lists.

## Rate limiting

`auth.rate_attempts` (default 5) and `auth.rate_window` (default 1m; env
`SOLTI_AUTH_RATE_ATTEMPTS`, `SOLTI_AUTH_RATE_WINDOW`) configure both failure
limiters, one for login and one shared by HTTP and gRPC discovery:
`rate_attempts` failures, each within `rate_window` of the previous one, block a
key for `rate_window`. Each limiter keeps at most 100 000 keys in memory; past
that, the least recently updated key is evicted.

| Surface | Counted per | Counts | Blocked answer |
| -- | -- | -- | -- |
| Login (`POST /login`) | account and client IP; a known device by itself | failed logins and logins in flight; a success resets the account, or the device | 429 |
| HTTP and gRPC discovery | client IP + agent ID, shared | agent token failures and token checks in flight | 429 / `ResourceExhausted` |

Every attempt reserves a slot on its keys before its check (password or agent
token), so a burst of parallel attempts gets at most `rate_attempts` checks per
key. A client IP is counted by its /64 prefix for IPv6; IPv4-mapped IPv6
addresses count as IPv4. Nothing resets a client IP key, and the User-Agent
plays no part. Agents behind one egress IP never block each other. The
`rate_limited` dashboard event is recorded once, when a login key (account,
client IP or device) becomes blocked.

The account lockout applies only to clients podium does not recognize. Every
successful login sets a device cookie (`device_token`: HttpOnly, SameSite
Strict, sent only to `/login`, Secure per `auth.cookie_secure`) signed for that
account with a key derived from `auth.jwt_secret`. A browser that logged in as
the account within the last 90 days sends it back, and its attempts keep their
own counter: failed logins from elsewhere, which can block the account (the
public `admin` included), do not lock that browser out, and its own
`rate_attempts` failures block only itself. A client without a valid cookie (new
or cleared, older than 90 days, issued for another account, or signed before the
JWT secret changed) counts per account and client IP.

To end a lockout early, restart podium: the limiters live in memory, so a
restart clears them. Behind a reverse proxy, list the proxy in
`trusted_proxies`; otherwise every client shares the proxy's IP and its IP key.

The main UI and API listener has no limit on failed authentication: its tokens
and session IDs cannot be guessed, and passwords go through the login limiter.

### Sync rate

Besides the failure limiter, each agent ID has a bucket of syncs on HTTP and
gRPC discovery. It holds 10, every sync takes one, and one comes back every
second. These are constants; there are no configuration keys. A sync that finds
the bucket empty is answered HTTP 429 with `Retry-After: 1` / gRPC
`ResourceExhausted`, and writes nothing. The SDK treats both as retryable.

The bucket is used twice. Once the payload is validated, and before the token is
checked, a sync looks at its agent's bucket without taking from it, and a sync
that finds it empty is refused at once, whatever token it carries: it opens no
transaction in the store and counts as no failure for the failure limiter. Then
the token is checked, and only after that is a token taken, before the agent is
written. So a sync with a wrong token, no token, or a payload that breaks a
limit takes nothing from the bucket of the agent whose ID it carries, and the
failure limiter counts the token failures. The look can race a concurrent sync,
so a sync that found a token may find none when it is charged, and is refused
then, having checked its token. The sync that enrolls an agent ID is charged one
token, like any other. A refused sync still costs its body read and its
validation. HTTP and gRPC share the buckets, so an agent has one budget
whichever transport it uses. With `agent_auth.require: false` there is no token
check, and the rate still applies.

Refusing before the token check shows the throttling state of an agent ID to a
caller that has not authenticated: a wrong token for an agent whose bucket is
empty gets 429, where a wrong token for an agent that has tokens left gets 401.
Nobody can drain a bucket that way, since only a sync that passed the token
check takes a token; what the 429 tells is that the agent ID has synced too
often just now. The WARN about a refusal can likewise be caused by a caller that
has not authenticated: it names the agent ID, not who sent the sync.

The buckets live in memory on the node that runs the discovery handlers. In raft
mode that is the leader, where followers forward the syncs they get. A restart
clears them, and after a failover the new leader counts from its own buckets,
which hold nothing for the syncs the former leader served. A bucket that was not
used for 10 minutes is dropped, so memory follows the agents that sync. The
first refused sync of an agent ID logs a WARN with the agent ID, and then at
most one more a minute for that ID. The access loggers record each refused sync
at DEBUG, one line for each, in place of the WARN of an HTTP 4xx or the ERROR of
a failed gRPC call. That is the level of the line and not the number of lines:
`log.level` defaults to `info`, which drops those lines, and at `debug` a DEBUG
line is written like any other, so an agent that keeps syncing writes one for
each refused sync. The HTTP 429 carries `Retry-After` and the gRPC status
carries `RetryInfo`, and the loggers take those for a throttle. A follower that
proxies or forwards a leader's refusal logs it at DEBUG too. A sync that fails
in another way keeps its level: a wrong token (WARN over HTTP, ERROR over gRPC)
and the 429 / `ResourceExhausted` of the failure limiter.

## First administrator

When the user table is empty, the first start creates the administrator
`user-admin` (login `admin`) and sets its password:

- `SOLTI_BOOTSTRAP_ADMIN_PASSWORD` sets it. This is its only source: the field
  has no YAML key, so a config file cannot carry it. It must pass the password
  policy (12 characters at least, 72 bytes at most); a value that breaks it
  fails startup with an error that names the variable and not the value. It is
  never written to a file or a log. When podium stores it, a
  `bootstrap-admin-password` file left by an earlier generated seed is removed
  and its path logged.
- Without it podium generates a password and writes it, on one line with mode
  0600, to `bootstrap-admin-password` in the data directory of the selected
  backend: `cluster.standalone.data_dir` (default `data/podium`), or
  `cluster.raft.data_dir` in raft mode, where the file sits on the node whose
  leadership term seeded, the node whose log line gives its `node_id`. The log
  names only that path, made absolute.
- In ephemeral mode (`cluster.standalone.ephemeral: true`) there is no data
  directory, so the generated password is logged once, at WARN, with the prefix
  `ephemeral mode:`. No other mode logs a password.

Once users exist the variable is ignored, with one WARN per start: remove it
from the environment after the first start. A raft node seeds at the start of
each of its leadership terms, whenever a quorum gives it one, and never waits
for a leader at start. It refuses a value that breaks the policy at start, even
with users present, and logs the WARN about an ignored value at every term. See
`internal/bootstrap/README.md`.

## Standalone storage

```yaml
cluster:
  standalone:
    data_dir: /var/lib/solti/podium   # default data/podium
    snapshot_threshold: 4096          # log entries that start a snapshot
```

The standalone backend keeps its state in `podium.db` under
`cluster.standalone.data_dir` (env `SOLTI_CLUSTER_STANDALONE_DATA_DIR`), unless
`cluster.standalone.ephemeral: true` keeps it in memory. A blank `data_dir`
without that flag fails configuration loading.

`cluster.standalone.snapshot_threshold` (env
`SOLTI_CLUSTER_STANDALONE_SNAPSHOT_THRESHOLD`, default 4096) is the number of
log entries after which the store writes its state as a snapshot and truncates
the log; 16 MiB of entries does the same. Zero keeps the default; a negative
value fails configuration loading. See `internal/storage/durable/README.md`.

## Cluster

`cluster.backend: raft` runs podium as one node of a raft cluster. Only that
backend reads `cluster.raft` and `cluster.discovery`: standalone ignores them,
valid or not. To set up and run a cluster, follow "Runbook: a three-node
cluster" in `internal/raft/README.md`: certificates, network, the first start,
restarts and failover, adding and removing members, the routing of browsers, and
upgrades. Every node serves reads from its own store, which can lag the
leader's: "Reads on a follower" there says what a lagging, cut-off or joining
node serves, revocations included.

```yaml
cluster:
  backend: raft
  raft:
    node_id: n1                     # unique, the same on every start
    bind_addr: 0.0.0.0:7000         # where the raft transport listens
    advertise_addr: 10.0.0.1:7000   # where peers dial it; default bind_addr
    data_dir: /var/lib/solti/raft
    join: false                     # true: a new node waits for the leader to add it
    election_timeout_ms: 1000       # 0 keeps hashicorp/raft's 1 s
    heartbeat_timeout_ms: 500       # 0 keeps hashicorp/raft's 1 s
    snapshot_interval: 120s         # how often to check whether to snapshot; 0s keeps 120s
    snapshot_threshold: 8192        # log entries since the last snapshot that start one; 0 keeps 8192
    trailing_logs: 10240            # log entries a snapshot leaves in the log; 0 keeps 10240
    tls:                            # mutual TLS between the nodes
      cert_file: /etc/solti/raft/n1.crt
      key_file: /etc/solti/raft/n1.key
      ca_file: /etc/solti/raft/ca.crt
  discovery:
    driver: static                  # the default; or dns
    peers:
      - n1=10.0.0.1:7000
      - n2=10.0.0.2:7000
      - n3=10.0.0.3:7000
```

Env equivalents: `SOLTI_CLUSTER_BACKEND`, `SOLTI_CLUSTER_RAFT_NODE_ID`,
`SOLTI_CLUSTER_RAFT_BIND_ADDR`, `SOLTI_CLUSTER_RAFT_ADVERTISE_ADDR`,
`SOLTI_CLUSTER_RAFT_DATA_DIR`, `SOLTI_CLUSTER_RAFT_JOIN`,
`SOLTI_CLUSTER_RAFT_ELECTION_TIMEOUT_MS`,
`SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS`,
`SOLTI_CLUSTER_RAFT_SNAPSHOT_INTERVAL`, `SOLTI_CLUSTER_RAFT_SNAPSHOT_THRESHOLD`,
`SOLTI_CLUSTER_RAFT_TRAILING_LOGS`, `SOLTI_CLUSTER_RAFT_TLS_CERT_FILE`,
`SOLTI_CLUSTER_RAFT_TLS_KEY_FILE`, `SOLTI_CLUSTER_RAFT_TLS_CA_FILE`,
`SOLTI_CLUSTER_RAFT_ALLOW_INSECURE_TRANSPORT`, `SOLTI_CLUSTER_DISCOVERY_DRIVER`,
and `SOLTI_CLUSTER_DISCOVERY_PEERS`, split on commas:

```sh
SOLTI_CLUSTER_DISCOVERY_PEERS="n1=10.0.0.1:7000, n2=10.0.0.2:7000, n3=10.0.0.3:7000"
```

`election_timeout_ms` and `heartbeat_timeout_ms` are raft's election and
heartbeat timeouts, in milliseconds; 0 keeps hashicorp/raft's default of 1 s.
Configuration loading refuses what hashicorp/raft would refuse at the start: a
timeout under 5 ms, and an election timeout below the heartbeat timeout, the
default of 1 s counted, so a `heartbeat_timeout_ms` over 1000 needs an
`election_timeout_ms` at least as long. It also refuses a value over
4611686018427 (about 146 years) as too large, naming that largest value:
hashicorp/raft's heartbeat and election timers wait up to twice the timeout in a
Go duration, which holds 9223372036854 whole milliseconds at most.

`snapshot_interval`, `snapshot_threshold` and `trailing_logs` set how a node
compacts its raft log. Every `snapshot_interval` (a duration string such as
`120s`, in YAML and in the environment) the node checks whether
`snapshot_threshold` entries have piled up since its last snapshot, and takes
one if so; the snapshot truncates the log down to its last `trailing_logs`
entries. A follower that is further behind than that catches up from a snapshot
of the whole state, not from log entries. The defaults are hashicorp/raft's:
`120s`, 8192 and 10240. Zero keeps the default; a negative value fails
configuration loading, and so does an interval under 5 ms, the least that
hashicorp/raft accepts, which has no other limit on the three settings. They are
not `cluster.standalone.snapshot_threshold`, which is the standalone store's.
hashicorp/raft's own logs, of its elections, snapshots and transport, go through
podium's logger like any other line, at their own INFO, WARN and ERROR levels,
with `profile=raft` and `component` (`raft`, `snapshot` or `raft-net`); its
TRACE and DEBUG lines are DEBUG lines, written at `log.level: debug` and dropped
above it, and it writes nothing to stderr. See `internal/raft/README.md`.

Each `discovery.peers` entry is `id=host:port`: a node's `node_id` and the
address its peers dial. A bare `host:port` names the node by its address. Spaces
around an entry, its ID and its address are trimmed. List every node, this one
included: a one-node cluster lists itself.

Every node must serve `http.addr`, `http_discovery.addr` and `grpc.addr` on the
same ports as the others (the same-port assumption): a follower knows the leader
by its raft address only, so it forwards a write, or a gRPC discovery sync, to
the host of the leader's raft advertise address with its own port of the
listener the request came in on, and names that address in the HTTP `X-Leader`
and the gRPC `x-leader` hints. Nothing checks the assumption: on a node whose
port differs, forwards go to a port the leader does not serve, and fail (HTTP
502, gRPC `Unavailable`), or reach another node that shares the leader's host. A
write, or a gRPC call, is forwarded once at most, so it never goes round nodes:
a follower that gets one another node forwarded answers HTTP 503 or gRPC
`Unavailable`, with the hint.

Configuration loading fails, and nothing in `data_dir` is opened, when:

- `raft.node_id`, `raft.bind_addr` or `raft.data_dir` is empty;
- the node has neither all three `raft.tls` files nor
  `raft.allow_insecure_transport: true`, or has the files and the flag (see
  "Raft transport security" below);
- `bind_addr` or `advertise_addr` is not `host:port` with a port from 1 to
  65535;
- `bind_addr` listens on every interface (`0.0.0.0`, `::` or no host) and
  `advertise_addr` is unset, or `advertise_addr` is such an address itself;
- a peer entry is malformed, an ID or an address appears twice, or no entry is
  this node: `node_id` at its advertise address (`advertise_addr`, or
  `bind_addr` without it), compared as written;
- an entry is bare and `node_id` is not this node's advertise address. To choose
  IDs, write every entry as `id=host:port`.

A node with `raft.join: true` asks discovery for nothing (see "Joining a
cluster" below), so of the last two rules only the syntax of each entry is
checked: its `peers` may leave it out, or be empty. With `discovery.driver: dns`
only `discovery.hostname` and `discovery.port` are checked for it.

With `discovery.driver: dns`, each address that `discovery.hostname` resolves to
is a peer at `ip:<discovery.port>`, named by that address. So
`discovery.hostname` and a `discovery.port` from 1 to 65535 are required, the
advertise address must be this node's IP and `discovery.port`, written as DNS
returns it (`10.0.0.1:7000`, `[fd00::1]:7000`), and `node_id` must be that same
address. Env: `SOLTI_CLUSTER_DISCOVERY_HOSTNAME`,
`SOLTI_CLUSTER_DISCOVERY_PORT`.

A node bootstraps the cluster on its first start, while `data_dir` holds no raft
state: it asks discovery for the peers and starts with exactly that list, sorted
by ID, so every node derives the same configuration whatever order discovery
reports the peers in. Every node must see the same peers at its first start, and
nothing compares the lists of different nodes: DNS answers that differ between
nodes go unnoticed and leave them with different configurations, while an
address DNS answers twice fails the start as a duplicate. A discovery error
fails that start, and so does a list that breaks the rules above, which DNS
answers can: a node that advertises a host name is not among them. A start that
fails on its peers keeps no raft state, so the next one bootstraps again. Later
starts load the configuration from the node's own log and do not ask discovery,
so a discovery outage does not keep a restarting node down; validation still
checks a static list on every start. See `internal/raft/README.md`.

So `discovery.driver: dns` is safe for a first start only when every node
resolves the complete set of nodes before its first start; prefer static peers
for the first start. A node that resolves only itself bootstraps a cluster of
one voter, which leads, seeds and takes writes on its own and refuses the votes
of the others, which never elect a leader: a fork that no error reports. On
Kubernetes, give the StatefulSet `podManagementPolicy: Parallel`, as its default
`OrderedReady` starts pod-0 alone, and its headless service
`publishNotReadyAddresses: true`, so that every pod resolves every pod, and its
own advertise host, which a node resolves at start, before it is ready. After a
first start, check `GET /api/v1/cluster` on every node: each must name the same
leader and list every node.

### Settings every replica must share

These settings must be the same on every node of a raft cluster, as a request
may reach any of them:

- `auth.jwt_secret` (env `SOLTI_AUTH_JWT_SECRET`), `auth.cookie_secure`,
  `auth.issuer` and `auth.audience`: a token one node signed fails on another,
  and cookies are marked otherwise;
- `agent_auth.require` and `agent_auth.enroll_secret` (env
  `SOLTI_AGENT_AUTH_ENROLL_SECRET` only): the nodes authenticate and enroll
  agents otherwise, derive other tokens, and send agents tokens or not;
- `trusted_proxies`: a proxy one node trusts is a client on another, and the
  limiters key its clients otherwise;
- `cors.*`: a browser origin one node allows, another refuses;
- `tls.server.client_ca_file`, which turns agent mTLS on for the discovery
  listeners: a node without it accepts agents that present no certificate, and a
  node with another CA refuses agents the others accept. What must match is
  whether it is set, and the certificates of the bundle, not its path, which may
  differ from node to node.

Every raft node digests them, group by group, and its `config-check` runner
compares the digests with its leader's at each change of leader. A node whose
settings differ logs one ERROR per leader,
`configuration check: this node's configuration differs from the leader's in settings that every replica must share`,
naming the groups that differ (`auth`, `agent auth`, `proxies`, `CORS`,
`agent TLS`) and never a value; a drifted leader is reported by each of its
followers. `GET /api/v1/cluster` shows each node's fingerprint of these settings
as `config_fingerprint`: nodes whose fingerprints differ do not share them.
Settings that apply alike count alike: `trusted_proxies` and
`cors.allow_origins` in any order, a proxy as an IP or its /32, a CORS list left
empty or set to the defaults it gets, an empty `auth.issuer` or `auth.audience`
and its default, and the same CA bundle at another path. Overlapping proxy
prefixes are not merged, though: write the lists alike on every node. A raft
node reads `tls.server.client_ca_file` before it starts: a bundle it cannot
read, or that holds no certificate, fails the start with the same error as it
does in standalone.

Whoever reads the fingerprint and knows the other settings can confirm a guess
of a weak `jwt_secret` or `enroll_secret` offline, as the fingerprint of every
guess can be computed. So both must be long random values, which is why the
fingerprint is served only behind `cluster:get` and is never logged.

Nothing compares the other settings. Those of the runners that only the leader
runs, `lifecycle.*`, `sync.*` and `history.*`, change what the cluster does each
time the leadership moves to a node configured otherwise. Of `tls.*`, each
node's `tls.server.cert_file` and `key_file` are its own, and `tls.client.*` is
not compared either: its drift shows as failed dials to agents or to the leader.
Other settings, such as the token lifetimes, may differ unseen too: keep every
node's configuration the same, and change it on all of them. See "Configuration
drift" in `internal/raft/README.md`.

### Joining a cluster

`cluster.raft.join: true` (env `SOLTI_CLUSTER_RAFT_JOIN`) starts a node whose
`data_dir` holds no raft state without a bootstrap: it asks discovery for
nothing, writes no configuration, logs one INFO line that it waits, and waits
for the leader of the running cluster to add it with
`POST /api/v1/cluster/members` (see "Cluster" in `internal/handler/README.md`).
A node that replaces a removed one starts with it, and so does a node whose
`data_dir` was wiped, once it was removed from the cluster (see the runbook in
`internal/raft/README.md`): without it, a first start bootstraps a configuration
of its own from the static list. Once the node holds raft state, a restart loads
the configuration from the node's own log, so leaving the flag set changes
nothing. Clearing it does: without it, configuration loading needs
`discovery.peers` to list the node as its `node_id` at its advertise address,
and with `discovery.driver: dns` needs `node_id` and the advertise address to be
the node's `ip:<discovery.port>`, as for any node. So keep the flag set until
`discovery.peers` lists the node, or with dns until its `node_id` matches too;
see "Replacing a dead member" in `internal/raft/README.md`.

### Raft transport security

The raft transport carries the whole control-plane state, agent tokens and
password verifiers included, so it runs mutual TLS 1.3 between the nodes:

- Each node has one certificate, `raft.tls.cert_file` with its
  `raft.tls.key_file`, and presents it both to the peers that connect to it and
  to the peers it dials. Every certificate of its chain, the root included, must
  allow both server and client authentication in its extended key usage, or
  carry no extended key usage.
- `raft.tls.ca_file` holds the cluster CA. A node accepts a connection only from
  a peer that presents a certificate this CA signed, and dials a peer only when
  the peer's certificate is signed by it too.
- A node dials a peer at its address as the peers list it, and the peer's
  certificate must name that host as a subject alternative name, compared as
  written, not as it resolves: an IP SAN for `10.0.0.2:7000`, a DNS SAN for
  `podium-1.podium:7000`. With `discovery.driver: dns` the peers are dialed at
  their IPs, so the certificates need IP SANs.
- TLS 1.2 and older are refused both ways.
- A node closes a connection it accepted whose TLS handshake does not finish
  within 10 s, so a host that connects to `bind_addr` and stalls holds nothing
  of the node for longer.

TLS keeps other hosts out of the cluster, not off the port: every connection
from a host that is no cluster node, refused or timed out in its handshake, is
one ERROR line in the node's log, `failed to decode incoming command`, and
nothing limits them. So `raft.bind_addr` must be reachable only from the
cluster's nodes, by firewall or network policy.

`raft.tls.ca_file` must be a CA that signs raft node certificates only, of one
cluster. A node accepts every certificate it verifies as a full member of the
cluster, whatever host the certificate names: it does not check a connecting
peer's certificate against the configured peers. So it must never be the agents'
CA, the CA of `tls.server` or `tls.client`, or a bundle merged with any of them,
or every certificate those sign could write the cluster's state; and two
clusters must never share a raft CA, or each takes the other's nodes for
members.

Removing a node revokes nothing: its certificate stays a full member's
credential for the raft port, and for the configuration request on it, until it
expires. Whoever holds its key can depose the leader with a higher term, and
name its own host as the leader, so that the followers forward writes, logins,
cookies and tokens included, to it. After removing a node whose keys may be
exposed, issue a new raft CA and new certificates to every node: that alone
stops the key, as no node accepts a certificate of the old CA any more.
Re-issuing the removed node's `tls.server` certificate does not: podium checks
no revocation, so the old certificate keeps passing the followers' check against
`tls.client.ca_file` until it expires. That matters when a replacement reuses
the node's host name or IP; then also issue every node a new `tls.server`
certificate from a new CA, add that CA to `tls.client.ca_file` and to what the
agents trust for the server, and take the old CA out of both: nothing else stops
the old certificate. `tls.client.ca_file` also verifies the agents' own
endpoints, as one client configuration serves the calls to agents and the
forwards to the leader, so it keeps the CA of those; a server CA that also
signed the agents' endpoints can leave only once those are re-issued too. A
compromised host holds more than its raft key: its other keys,
`auth.jwt_secret`, `agent_auth.enroll_secret`, which derives the token of every
agent enrolled under it, and in `data_dir` the raw tokens of the stored
credentials, the password hashes, and the task specs, with whatever an operator
put in a task's command or config. "A compromised node" in
`internal/raft/README.md` lists everything podium keeps on a node, with each
rotation and its cost; see also "Transport security" there.

The files are read when the node starts, before anything in `data_dir` is
opened: a file that is missing or holds no usable certificate or key fails the
start, and a renewed certificate takes effect at the next start. The node then
checks its own certificate as its peers will: signed by the CA of `ca_file`,
valid now, for both server and client authentication, and naming the host of its
advertise address as written. A certificate that fails any of these fails the
start with an error that says which, so a node that its peers would refuse does
not start cut off from them.

`raft.allow_insecure_transport: true` runs the transport on plain TCP instead,
for a cluster on a trusted network: any host that reaches `bind_addr` can then
read the raft traffic and forge it, and every start logs one WARN that says so.
The flag runs plain TCP only for a node without `raft.tls`; with both set,
configuration loading fails.

### Raft data directory

`data_dir` holds the raft log, the stable store and the snapshots, so neither
`data_dir` nor anything in it may carry group or other permission bits. A node
makes a missing `data_dir` and its `snapshots` directory with mode 0700, and
refuses to start, before it opens anything there, when either is open to its
group or others or is a symlink, written with a trailing slash or not, as the
standalone store refuses its directory. The BoltDB files are 0600. Each snapshot
directory is made 0700 and its files 0600 as soon as hashicorp/raft creates
them, before any state is written into them, and they stay so once the snapshot
is in place, a snapshot a leader installs on the node included; snapshots
already there, such as an earlier version's, are made private at start. Volume
settings that add group permissions to `data_dir` make the node refuse to start.

### Upgrading a raft node

Earlier versions appended a node to its own peers: an empty `peers` list
bootstrapped a one-node cluster, and a list of only the other nodes worked. This
version refuses both. Before upgrading a raft node, add it to its own `peers` as
`<node_id>=<advertise address>`; a node that keeps bare entries needs `node_id`
equal to its advertise address. Do this on every node. A list that already names
the node at its advertise address, as written, passes unchanged. A node whose
list does not pass is refused at configuration loading, before anything in
`data_dir` is opened, so its raft state stays as it was: fix the list and start
it again. A node with raft state reads its configuration from its own log, not
from `peers`, so listing itself changes nothing in the running cluster. A node
with raft state keeps its `node_id` and its advertise address: started under
another ID or address, it is not the member its peers know, and logs one ERROR
at start,
`raft: this node is not in its stored raft configuration as its node_id at its advertise_addr`;
under another ID it is never elected. So do not rename the nodes of an earlier
version, `n1` for `10.0.0.1:7000` say, during the upgrade: a renamed or
re-addressed node must be removed from the cluster, its `data_dir` wiped, and
added again with `join: true`. The two addresses are compared as written, not as
they resolve: a member whose stored address spells its advertise address
otherwise, a name against an IP say, logs the ERROR at every restart although it
works, as its peers dial the stored address and raft elects it by ID; removing
it and adding it again under the spelling of its advertise address, as above,
clears the ERROR.

Earlier versions also ran the transport on plain TCP and made `data_dir` and
`snapshots` 0755 and the snapshot files 0644. This version needs, on every node:

- `raft.tls`, with a certificate as above for each node, of a CA kept for raft
  alone, or `raft.allow_insecure_transport: true`, which keeps plain TCP as
  before. A node on TLS and a node on plain TCP cannot reach each other, so a
  cluster works only while a majority of its nodes runs the same transport, and
  an upgrade keeps plain TCP: upgrade every node with
  `allow_insecure_transport: true`, one at a time to a version that still writes
  version 3, or all at once to one that writes version 4 (the last paragraph of
  this section), then switch all nodes to `raft.tls` at once, in one full stop
  and start, during which the cluster takes no write (see "Rolling upgrade" in
  the runbook of `internal/raft/README.md`).
- `chmod -R go-rwx` on `data_dir`. A node refuses a `data_dir` or `snapshots`
  directory that is still open to the group or others; it makes the snapshots in
  it private by itself at its next start.

This version also adds the permissions `cluster:get` and `cluster:manage`, in
standalone too, which the Admin role gets when bootstrap reconciles it, and a
user who holds every permission directly, not through the Admin role, needs the
two new permissions to keep counting as an administrator.

Versions from the one with the credential reset and the agent delete add the
permissions `agents:reset` and `agents:delete` the same way, in standalone too:
the Admin role gets them when bootstrap reconciles it, no other built-in role
holds them, and a user who holds every permission directly, not through the
Admin role, does not gain them and needs both to keep counting as an
administrator.

This version's nodes check their settings against the leader's (see "Settings
every replica must share" above), which a node of an earlier version cannot
answer. During a rolling upgrade to a version that has the check and still
writes version 3 (an upgrade to one that writes version 4 is one full stop and
start, below, after which no node of an earlier version runs), each upgraded
follower of a leader that still runs the earlier version logs one WARN,
`configuration check: cannot fetch the leader's configuration digests`, and
fetches again 1, 2, 4, 8, 16 and 32 s after each failure, then every 60 s, from
1 s again at each change of leader. The old leader logs one ERROR,
`failed to decode incoming command` with `unknown rpc type 240`, for each of
those attempts of each such follower. Both stop once a node of this version
leads.

Versions from the one with derived agent credentials (see "Enrollment secret"
above) on write the raft log and the snapshots in version 4 of the command and
snapshot codecs, and earlier versions read version 3 at most: mixed versions are
unsupported. Stop every node, upgrade them all and start them again, in one full
stop and start, during which the cluster takes no write. A node of an earlier
version cannot apply what a node of this version proposes: it drops each
command, logs
`raft: committed command is not decodable and was dropped on this replica (corrupted entry or version skew)`
and diverges, and started again it refuses its own log with
`unsupported command version 4`. Rolling back needs a backup of `data_dir` taken
before the upgrade, for the same reason. A node whose snapshots or log still
hold version-2 entries is refused at start too; "Rolling upgrade" in the runbook
of `internal/raft/README.md` says what to run first.

## Observed run history

```yaml
history:
  archive:
    retention: 168h
    max_records: 10000
    max_bytes: 33554432
  collector:
    tick_interval: 5s
    cycle_timeout: 8s
    max_concurrency: 2
    max_agents_per_cycle: 100
    page_size: 256
    max_pages: 4
    max_run_continuations: 1024
```

Environment equivalents include `SOLTI_HISTORY_ARCHIVE_RETENTION`,
`SOLTI_HISTORY_ARCHIVE_MAX_RECORDS`, `SOLTI_HISTORY_ARCHIVE_MAX_BYTES` and
`SOLTI_HISTORY_COLLECTOR_MAX_CONCURRENCY`. YAML and ENV use duration strings.
Zero values inherit owning-package defaults; negative values and collector
counts above their supported bounds fail configuration loading. `max_bytes`
measures conservative retained-record payload, not database/RSS size. Captures
are best effort and bound each task buffer to 8 MiB in addition to page/time
limits.

Retention is enforced from counters of the archive's rows, bytes and oldest
capture time, not by reading the archive: the collector scans it only when a
limit is crossed. A node that takes leadership counts it again once, since
another node may have written to it. A row ages from the time it was captured.

## Event stream

```yaml
events:
  coalesce: 1s    # env SOLTI_EVENTS_COALESCE
```

`events.coalesce` (default 1s) is the window in which the event hub coalesces
the refresh pulses it sends to the browsers' event streams (`agent_update`,
`spec_update`, `dashboard_update`, `user_update`, `session_update`): per kind,
the first pulse after a quiet period goes out at once and the pulses that follow
within the window become one trailing delivery at its end, so a burst reaches a
browser as two pulses, and its last change is never lost. It is also the longest
a pulse can wait. Zero keeps the default; a negative value fails configuration
loading. The keepalive and the access re-check of the stream are not pulses and
are not delayed. In raft mode each replica coalesces for its own browsers. See
`internal/event/README.md`.

## Agent lifecycle

```yaml
lifecycle:
  tick_interval: 10s
  default_heartbeat: 30s    # for agents that report none
  inactive_multiplier: 2    # silence over 2 × heartbeat: inactive
  disconnect_multiplier: 5  # over 5 × heartbeat: disconnected
  delete_multiplier: 10     # deleted past the larger of 10 × heartbeat ...
  delete_after: 24h         # ... and this
  max_concurrency: 4
```

`delete_after` is a floor: an agent is deleted once its silence exceeds the
larger of `delete_after` and `delete_multiplier` × its heartbeat, and never
sooner. Silence counts from the later of the agent's last sync and the start of
the replica's leadership term, so after a restart, or a change of leader in raft
mode, every agent that has not synced yet gets the full thresholds to sync again
before its status changes. Zero values inherit the defaults shown; negative
values fail loading. See `internal/server/README.md` for the states and the
grace of each term.

Deleting an agent drops its row and the labels operators set on it. Its rollouts
and its credential stay: until the agent syncs again, its pushes fail with
`agent not found` until `max_retries`, and its synced rollouts are skipped. When
it syncs again, with its token, it comes back without labels, so the next Deploy
of a spec that targets it by labels turns its rollout into an uninstall unless
the labels are restored first.

## Rollout sync

```yaml
sync:
  tick_interval: 10s
  push_timeout: 15s    # budget of each agent call, and of an agent's turn per tick
  max_concurrency: 4   # agents worked on at once
  max_retries: 5       # failed pushes before a rollout waits for its agent
```

A failed rollout is pushed again after `tick_interval` × 2^(attempts−1), at most
5 minutes; after `max_retries` failures it waits until its agent reconnects or
the spec is deployed again. An agent that leaves a call unanswered holds one of
the `max_concurrency` slots for one `push_timeout`, then gets no call until its
own backoff passes: the same schedule, counted in unanswered calls and kept in
memory. The backoff ends sooner when the agent row shows a reconnect: the agent
active again after it was inactive or disconnected, or with a lower uptime. An
agent that stays active keeps it. Each agent's turn in a tick starts calls for
one `push_timeout` of wall time at most; what is left waits for the next tick,
so an agent that answers slowly holds a tick for about one `push_timeout`, not
one per rollout. The check that an agent still has its tasks is one listing of
them, up to 1000 to a page (fewer when the agent's answer would be too large),
and only a whole listing counts: an agent whose last page cannot start inside
what is left of one `push_timeout` after its pushes, or whose listing passes the
bounds in the server README (65536 task IDs, 32 MiB of them, 1024 calls), is not
checked. Zero values inherit the defaults shown; negative values fail loading.
See `internal/server/README.md` for the scheduling.
