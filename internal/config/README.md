# Package config

Package config aggregates per-package configuration structs and provides a
single `Default()` constructor for development use:

- **Config** — top-level struct embedding sub-configs from `httpserver`,
  `grpcserver`, `lifecycle`, `sync`, `server`, `auth`, and `trigger`.
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

Entries are IP addresses or CIDR prefixes, trimmed of spaces, so
`SOLTI_TRUSTED_PROXIES="10.0.0.1, 10.0.0.2"` works; anything else, an empty
entry included, fails configuration loading. The headers count only when the
TCP peer is in the list:

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

## Browser sessions

`auth.cookie_secure` (env `SOLTI_AUTH_COOKIE_SECURE`) decides when the three
auth cookies carry the `Secure` attribute, on every response that sets or
clears them (login, logout, refresh, a revoked session), and so does the device
cookie that login sets (see "Rate limiting"):

- `auto` (default): when the client used HTTPS, either a TLS connection or
  `X-Forwarded-Proto: https` from a proxy in `trusted_proxies`.
- `always`: on every response. Use it when the proxy that terminates TLS cannot
  be listed in `trusted_proxies`.

Any other value, an empty one included, fails configuration loading.

The main listener refuses state-changing requests that a browser sends from
another origin (Go's `http.CrossOriginProtection`) with 403
`Cross-origin request blocked.`: a request whose `Sec-Fetch-Site` is not
`same-origin` or `none`, or, from a browser that does not send that header, one
whose `Origin` does not match `Host`. GET, HEAD and OPTIONS always pass, as do
clients that send neither header (scripts, agents). Behind a proxy that
rewrites `Host`, such old browsers are refused. The check runs before any
route, so a refusal gets no request ID or access-log line; it is logged at WARN
with the method, path, `Sec-Fetch-Site` and `Origin`. The discovery listener
has no such check.

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
with a mutation, and cannot be combined with `allow_credentials`.

## Rate limiting

`auth.rate_attempts` (default 5) and `auth.rate_window` (default 1m; env
`SOLTI_AUTH_RATE_ATTEMPTS`, `SOLTI_AUTH_RATE_WINDOW`) configure both failure
limiters, one for login and one shared by HTTP and gRPC discovery:
`rate_attempts` failures, each within `rate_window` of the previous one, block a
key for `rate_window`. Each limiter keeps at most 100 000 keys in memory; past
that, the least recently updated key is evicted.

| Surface                 | Counted per                                     | Counts                                                                          | Blocked answer            |
|-------------------------|-------------------------------------------------|---------------------------------------------------------------------------------|---------------------------|
| Login (`POST /login`)   | account and client IP; a known device by itself | failed logins and logins in flight; a success resets the account, or the device | 429                       |
| HTTP and gRPC discovery | client IP + agent ID, shared                    | agent token failures and token checks in flight                                 | 429 / `ResourceExhausted` |

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
`rate_attempts` failures block only itself. A client without a valid cookie
(new or cleared, older than 90 days, issued for another account, or signed
before the JWT secret changed) counts per account and client IP.

To end a lockout early, restart podium: the limiters live in memory, so a
restart clears them. Behind a reverse proxy, list the proxy in
`trusted_proxies`; otherwise every client shares the proxy's IP and its IP key.

The main UI and API listener has no limit on failed authentication: its tokens
and session IDs cannot be guessed, and passwords go through the login limiter.

## First administrator

When the user table is empty, the first start creates the administrator
`user-admin` (login `admin`) and sets its password:

- `SOLTI_BOOTSTRAP_ADMIN_PASSWORD` sets it. This is its only source: the field
  has no YAML key, so a config file cannot carry it. It must pass the password
  policy (12 characters at least, 72 bytes at most); a value that breaks it fails
  startup with an error that names the variable and not the value. It is never
  written to a file or a log. When podium stores it, a `bootstrap-admin-password`
  file left by an earlier generated seed is removed and its path logged.
- Without it podium generates a password and writes it, on one line with mode
  0600, to `bootstrap-admin-password` in the data directory of the selected
  backend: `cluster.standalone.data_dir` (default `data/podium`), or
  `cluster.raft.data_dir` in raft mode, where the file sits on the node that led
  at first boot. The log names only that path.
- In ephemeral mode (`cluster.standalone.ephemeral: true`) there is no data
  directory, so the generated password is logged once, at WARN, with the prefix
  `ephemeral mode:`. No other mode logs a password.

Once users exist the variable is ignored, with one WARN per start: remove it from
the environment after the first start. See `internal/bootstrap/README.md`.

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
