# Package config

Package config aggregates per-package configuration structs and provides a
single `Default()` constructor for development use:

- **Config** — top-level struct embedding sub-configs from `httpserver`,
  `grpcserver`, `lifecycle`, `sync`, `server`, `wire` (auth), and `trigger`.
- **Default()** — returns safe development defaults. Zero-valued sub-configs
  inherit package-level defaults via each package's `withDefaults()`.

## Adding a new sub-config

1. Add a `Config` struct with `withDefaults()` in the owning package.
2. Add the field to `config.Config`.
3. Optionally set non-zero defaults in `Default()`.
4. Pass `cfg.<Field>` in the `internal/app` composition root.

## Loading from external sources

`Load()` reads in priority order: `Default()` → YAML file (`--config` / `CONFIG_PATH`)
→ env (`SOLTI_` prefix), then enforces `Config.Validate()`.

## TLS

`config.TLS` is one shared block applied to **all** transports (mirrors the SDK's
`solti-tls`). Both halves are opt-in — empty means plaintext.

- `tls.server` (`cert_file`, `key_file`, `client_ca_file`) — the CP's serving
  identity for the HTTP, HTTP-discovery and gRPC listeners. Setting
  `client_ca_file` turns on **mTLS** (client cert required).
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

## Agent authentication

`agent_auth.require` defaults to `true`. Discovery enrolls each agent's bearer
token on first contact and answers a later sync without the same token with
HTTP 401 / gRPC `Unauthenticated`. Outbound calls to agents present the stored
token. SDK agents send their token only over TLS unless the agent explicitly
allows insecure token transport.

```yaml
agent_auth:
  require: false   # env SOLTI_AGENT_AUTH_REQUIRE=false
```

`require: false` turns off both directions: no token is checked and none is
sent. Startup then logs one WARN line: `agent authentication disabled: any host
that reaches the discovery listeners can register agents`.

## Client IPs and trusted proxies

`trusted_proxies` lists the reverse proxies whose forwarding headers podium
believes. It defaults to empty: the TCP peer is the client.

```yaml
trusted_proxies:   # env SOLTI_TRUSTED_PROXIES=10.0.0.1,10.1.0.0/16
  - 10.0.0.1
  - 10.1.0.0/16
```

Entries are IP addresses or CIDR prefixes; anything else fails configuration
loading. The headers count only when the TCP peer is in the list:

- `X-Forwarded-For`: all header lines are joined in order and walked from the
  right. Trusted entries are skipped; the first untrusted entry is the client.
  When every entry is trusted, the left-most one is. A missing or empty header,
  or an entry the walk cannot parse, leaves the peer as the client. Entries may
  be `ip`, `ip:port` or `[ip]:port`.
- `X-Forwarded-Proto`: the first value of the first header line, compared with
  `https` case-insensitively, says whether the client used HTTPS.

RFC 7239 `Forwarded` is not supported: behind a proxy that sends only
`Forwarded`, the proxy is the client. List only proxies that append the peer
to `X-Forwarded-For` (or replace the header); a listed host that passes a
client's header through unchanged lets that client pick its own IP. In raft
mode, list the replicas too: a follower forwards writes to the leader, which
otherwise sees the follower as the client.

gRPC discovery ignores this setting and always uses the peer address.

## Rate limiting

`auth.rate_attempts` (default 5) and `auth.rate_window` (default 1m; env
`SOLTI_AUTH_RATE_ATTEMPTS`, `SOLTI_AUTH_RATE_WINDOW`) configure every failure
limiter: `rate_attempts` failures, each within `rate_window` of the previous
one, block a key for `rate_window`. Each surface has its own in-memory limiter
of at most 100 000 keys; past that, the least recently updated key is evicted.

| Surface               | Counted per            | Counts                                           | Blocked answer        |
|-----------------------|------------------------|--------------------------------------------------|-----------------------|
| Login (`POST /login`) | account, and client IP | failed logins; a success resets the account only | 429                   |
| Main HTTP listener    | client IP              | 401 responses only                               | 429 for every request |
| HTTP discovery        | client IP + agent ID   | agent token failures                             | 429                   |
| gRPC discovery        | peer IP + agent ID     | agent token failures                             | `ResourceExhausted`   |

A client IP is counted by its /64 prefix for IPv6; IPv4-mapped IPv6 addresses
count as IPv4. Nothing resets a client IP key, and the User-Agent plays no
part. Agents behind one egress IP never block each other. The `rate_limited`
dashboard event is recorded once, when a login key becomes blocked.

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
Zero values inherit owning-package defaults; negative values and collector counts
above their supported bounds fail configuration loading. `max_bytes` measures
conservative retained-record payload, not database/RSS size. Captures are best
effort and bound each task buffer to 8 MiB in addition to page/time limits.
