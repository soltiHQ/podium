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

In raft mode a follower forwards the writes it gets on the HTTP and
HTTP-discovery listeners to the leader, at the host of the leader's raft
advertise address, as written, and this node's port of that listener. With
`tls.server` set it forwards over TLS with `tls.client`: `ca_file` must also
hold the CA of the replicas' server certificates, which must name each
replica's raft advertise host as written, an IP SAN for `10.0.0.1:7000` and a
DNS SAN for `podium-0.podium:7000`, and `cert_file`/`key_file` are presented
when the leader asks for a client certificate, as the discovery listener does
with `client_ca_file`. Without a `tls.client.ca_file`, the leader is verified
against the system roots. A leader whose certificate does not verify gets no
write: the follower answers 502. The raft transport has TLS settings of its
own, with a CA of its own, see "Raft transport security".

## Agent authentication

`agent_auth.require` defaults to `true`. It authenticates enrolled agents: a
sync for an agent ID that has a token, but without that token, gets HTTP 401 /
gRPC `Unauthenticated`, and outbound calls to the agent present the stored
token. By itself it does not keep other hosts out: the first sync of an unknown
agent ID enrolls the token it presents (trust on first use) and sets the
endpoint podium will call. Close enrollment with an enrollment secret (see
"Enrollment secret" below) or with client certificates
(`tls.server.client_ca_file`), or keep the discovery listeners
(`http_discovery.addr`, default `:8082`, and `grpc.addr`, default `:50051`)
reachable only from agent networks. With neither a secret nor a client CA,
startup logs one WARN line, with the two listen addresses: `agent
authentication covers enrolled agents only: the first sync of an unknown agent
ID enrolls it and sets the endpoint podium calls, so set
SOLTI_AGENT_AUTH_ENROLL_SECRET or tls.server.client_ca_file, or keep the
discovery listeners reachable only from agent networks`.

With `require: true`, a token longer than 1024 bytes is refused before any
check, with HTTP 400 `Agent token is too long.` / gRPC `InvalidArgument`, and
nothing is enrolled. The cap applies to what agents send, never to stored
state: a credential an older version stored with a longer token still loads, so
podium starts, but that agent's syncs are refused with 400, and no API resets a
stored credential yet. gRPC discovery caps request headers at 16 KiB, like the
HTTP listeners.

SDK agents send their token only over TLS unless the agent explicitly allows
insecure token transport (`allow_insecure_token_transport()`). With `require:
true`, a sync without a token is refused like a wrong one and logged at WARN
with the agent ID and the three ways out: TLS, so the SDK sends its token; the
SDK's insecure-token option; or `agent_auth.require: false`. With an
enrollment secret set, the line names `podium agent-token` instead of
`agent_auth.require: false`, which fails validation next to a secret. The
discovery limiter bounds these lines to `auth.rate_attempts` per
`auth.rate_window` for each client IP and agent ID.

```yaml
agent_auth:
  require: false   # env SOLTI_AGENT_AUTH_REQUIRE=false
```

`require: false` turns off both directions: no token is checked and none is
sent. Startup then logs one WARN line: `agent authentication disabled: any host
that reaches the discovery listeners can register agents`.

### Enrollment secret

`SOLTI_AGENT_AUTH_ENROLL_SECRET` closes enrollment. It is env-only: the field
has no YAML key, so a config file cannot carry it. It needs 32 bytes at least
and is used as is, without trimming; an empty value counts as unset. A shorter
one fails configuration loading with an error that names the variable and not
the value. So does setting it while `agent_auth.require` is `false`, where it
would close nothing.

With the secret set, the first sync of an unknown agent ID enrolls only if its
bearer token is the lowercase hex of HMAC-SHA256 of the agent ID under the
secret (64 characters). Any other token, a missing one included, is refused
like a wrong one: HTTP 401 / gRPC `Unauthenticated`, nothing is enrolled, and
it counts as a failure of the discovery limiter (see "Rate limiting"). A wrong
token is also logged at WARN with the agent ID and the client IP, never the
token, bounded by the discovery limiter like the WARN for a missing token. A
token over 1024 bytes still gets the 400 above. Agents that are already enrolled are
unaffected: their stored token is compared as before, so the secret revokes
nobody, and changing or removing it leaves them enrolled. No API resets a
stored credential yet. In raft mode each replica reads its own environment:
give every replica the same secret.

To provision an agent, derive its token on a host that has the secret, then
give the token to the agent as its bearer token. The agent must run with the
same ID:

```sh
export SOLTI_AGENT_AUTH_ENROLL_SECRET=...   # the value podium runs with
podium agent-token agent-1                   # prints the token and a newline
```

In the agent, pass it to the SDK with `with_token(Token::new(token)?)`, or
load it with `Token::from_env(...)`. Podium then presents the same token on its
calls to that agent. The SDK still sends it over TLS only, unless the agent
opts in with `allow_insecure_token_transport()`. Agents never see the secret,
only their own token: a leaked token lets nobody enroll another ID.

Derive a token only for an agent ID that is not enrolled yet. An enrolled agent
is checked against the token it enrolled with, so re-provisioning it with a
derived token that differs (it enrolled before the secret was set, or under
another secret) locks it out: every sync it makes gets 401, and no API resets a
stored credential. That includes an agent that lifecycle deleted: deletion
drops the agent row but keeps its credential, so the ID stays enrolled.
Instead, keep the agent's existing token, or, if that token is lost, run the
agent under a new agent ID and derive the token for that one.

`podium agent-token <agent-id>` must come first among the arguments. It checks
the ID like discovery does (1 to 128 bytes of `[A-Za-z0-9._-]`) and reads the
secret from the environment only. It never prints the secret, and exits non-zero
with one short line when the secret is missing or shorter than 32 bytes, or the
ID is invalid. It needs no configuration file. A bare `-h` or `--help` prints
the usage and exits 0; for an agent ID that starts with `-`, put `--` before
it: `podium agent-token -- -h`.

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

`auth.rotate_refresh` (env `SOLTI_AUTH_ROTATE_REFRESH`, default `false`) makes
every refresh of a browser's session replace its refresh token. The replacement
is a write, so in raft mode only the leader can make it. A browser whose access
token has expired (`auth.access_ttl`, 15m by default) and whose request, a GET
included, reaches a follower gets 503 with the HTTP address of the leader in
`X-Leader` instead of a new token, and sets no cookie; the follower logs one
WARN, `auth refresh needs the leader`, with the method and the path. So with
rotation on, a raft deployment needs leader-sticky routing: its load balancer
must send browsers to the leader, or retry a request that gets `X-Leader` on
the node it names. Without rotation a refresh only reads, and works on every
node, as does a request whose access token is still valid. Standalone is
unaffected. See `internal/raft/README.md`.

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
  at first boot. The log names only that path, made absolute.
- In ephemeral mode (`cluster.standalone.ephemeral: true`) there is no data
  directory, so the generated password is logged once, at WARN, with the prefix
  `ephemeral mode:`. No other mode logs a password.

Once users exist the variable is ignored, with one WARN per start: remove it from
the environment after the first start. See `internal/bootstrap/README.md`.

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
valid or not.

```yaml
cluster:
  backend: raft
  raft:
    node_id: n1                     # unique, the same on every start
    bind_addr: 0.0.0.0:7000         # where the raft transport listens
    advertise_addr: 10.0.0.1:7000   # where peers dial it; default bind_addr
    data_dir: /var/lib/solti/raft
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
`SOLTI_CLUSTER_RAFT_DATA_DIR`, `SOLTI_CLUSTER_RAFT_ELECTION_TIMEOUT_MS`,
`SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS`, `SOLTI_CLUSTER_RAFT_SNAPSHOT_INTERVAL`,
`SOLTI_CLUSTER_RAFT_SNAPSHOT_THRESHOLD`, `SOLTI_CLUSTER_RAFT_TRAILING_LOGS`,
`SOLTI_CLUSTER_RAFT_TLS_CERT_FILE`, `SOLTI_CLUSTER_RAFT_TLS_KEY_FILE`,
`SOLTI_CLUSTER_RAFT_TLS_CA_FILE`, `SOLTI_CLUSTER_RAFT_ALLOW_INSECURE_TRANSPORT`,
`SOLTI_CLUSTER_DISCOVERY_DRIVER`, and `SOLTI_CLUSTER_DISCOVERY_PEERS`, split
on commas:

```sh
SOLTI_CLUSTER_DISCOVERY_PEERS="n1=10.0.0.1:7000, n2=10.0.0.2:7000, n3=10.0.0.3:7000"
```

`election_timeout_ms` and `heartbeat_timeout_ms` are raft's election and
heartbeat timeouts, in milliseconds; 0 keeps hashicorp/raft's default of 1 s.
An election timeout below the heartbeat timeout passes configuration loading
but fails the start.

`snapshot_interval`, `snapshot_threshold` and `trailing_logs` set how a node
compacts its raft log. Every `snapshot_interval` (a duration string such as
`120s`, in YAML and in the environment) the node checks whether
`snapshot_threshold` entries have piled up since its last snapshot, and takes
one if so; the snapshot truncates the log down to its last `trailing_logs`
entries. A follower that is further behind than that catches up from a snapshot
of the whole state, not from log entries. The defaults are hashicorp/raft's:
`120s`, 8192 and 10240. Zero keeps the default and a negative value fails
configuration loading; an interval under 5 ms passes loading but fails the
start. They are not `cluster.standalone.snapshot_threshold`, which is the
standalone store's. hashicorp/raft's own logs, of its elections, snapshots and
transport, go through podium's logger like any other line, at their own INFO,
WARN and ERROR levels, with `profile=raft` and `component` (`raft`, `snapshot`
or `raft-net`); its DEBUG lines are dropped and it writes nothing to stderr. See
`internal/raft/README.md`.

Each `discovery.peers` entry is `id=host:port`: a node's `node_id` and the
address its peers dial. A bare `host:port` names the node by its address.
Spaces around an entry, its ID and its address are trimmed. List every node,
this one included: a one-node cluster lists itself.

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
- an entry is bare and `node_id` is not this node's advertise address. To
  choose IDs, write every entry as `id=host:port`.

With `discovery.driver: dns`, each address that `discovery.hostname` resolves
to is a peer at `ip:<discovery.port>`, named by that address. So
`discovery.hostname` and a `discovery.port` from 1 to 65535 are required, the
advertise address must be this node's IP and `discovery.port`, written as DNS
returns it (`10.0.0.1:7000`, `[fd00::1]:7000`), and `node_id` must be that same
address. Env: `SOLTI_CLUSTER_DISCOVERY_HOSTNAME`,
`SOLTI_CLUSTER_DISCOVERY_PORT`.

A node bootstraps the cluster on its first start, while `data_dir` holds no
raft state: it asks discovery for the peers and starts with exactly that list,
sorted by ID, so every node derives the same configuration whatever order
discovery reports the peers in. Every node must see the same peers at its
first start, and nothing compares the lists of different nodes: DNS answers
that differ between nodes go unnoticed and leave them with different
configurations, while an address DNS answers twice fails the start as a
duplicate. A discovery error fails that start, and so does a list that breaks
the rules above, which DNS answers can: a node that advertises a host name is
not among them. A failed start keeps no raft state, so the next one bootstraps
again. Later starts load the configuration from the node's own log and do not
ask discovery, so a discovery outage does not keep a restarting node down;
validation still checks a static list on every start. See
`internal/raft/README.md`.

### Raft transport security

The raft transport carries the whole control-plane state, agent tokens and
password verifiers included, so it runs mutual TLS 1.3 between the nodes:

- Each node has one certificate, `raft.tls.cert_file` with its
  `raft.tls.key_file`, and presents it both to the peers that connect to it
  and to the peers it dials. Every certificate of its chain, the root
  included, must allow both server and client authentication in its extended
  key usage, or carry no extended key usage.
- `raft.tls.ca_file` holds the cluster CA. A node accepts a connection only
  from a peer that presents a certificate this CA signed, and dials a peer only
  when the peer's certificate is signed by it too.
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

`raft.tls.ca_file` must be a CA that signs raft node certificates only. A node
accepts every certificate it verifies as a full member of the cluster, whatever
host the certificate names: it does not check a connecting peer's certificate
against the configured peers. So it must never be the agents' CA, the CA of
`tls.server` or `tls.client`, or a bundle merged with any of them, or every
certificate those sign could write the cluster's state.

The files are read when the node starts, before anything in `data_dir` is
opened: a file that is missing or holds no usable certificate or key fails the
start, and a renewed certificate takes effect at the next start. The node then
checks its own certificate as its peers will: signed by the CA of `ca_file`,
valid now, for both server and client authentication, and naming the host of
its advertise address as written. A certificate that fails any of these fails
the start with an error that says which, so a node that its peers would refuse
does not start cut off from them.

`raft.allow_insecure_transport: true` runs the transport on plain TCP instead,
for a cluster on a trusted network: any host that reaches `bind_addr` can then
read the raft traffic and forge it, and every start logs one WARN that says
so. The flag runs plain TCP only for a node without `raft.tls`; with both set,
configuration loading fails.

### Raft data directory

`data_dir` holds the raft log, the stable store and the snapshots, so neither
`data_dir` nor anything in it may carry group or other permission bits. A node
makes a missing `data_dir` and its `snapshots` directory with mode 0700, and
refuses to start, before it opens anything there, when either is open to its
group or others or is a symlink, written with a trailing slash or not, as the
standalone store refuses its directory. The BoltDB files are 0600. Each
snapshot directory is made 0700 and its files 0600 as soon as hashicorp/raft
creates them, before any state is written into them, and again once the
snapshot is in place; snapshots already there, such as an earlier version's,
are made private at start. Volume settings that add group permissions to
`data_dir` make the node refuse to start.

### Upgrading a raft node

Earlier versions appended a node to its own peers: an empty `peers` list
bootstrapped a one-node cluster, and a list of only the other nodes worked.
This version refuses both. Before upgrading a raft node, add it to its own
`peers` as `<node_id>=<advertise address>`; a node that keeps bare entries
needs `node_id` equal to its advertise address. Do this on every node. A list
that already names the node at its advertise address, as written, passes
unchanged. A node whose list does not pass is refused at configuration
loading, before anything in `data_dir` is opened, so its raft state stays as
it was: fix the list and start it again. A node with raft state reads its
configuration from its own log, not from `peers`, so listing itself changes
nothing in the running cluster.

Earlier versions also ran the transport on plain TCP and made `data_dir` and
`snapshots` 0755 and the snapshot files 0644. This version needs, on every
node:

- `raft.tls`, with a certificate as above for each node, of a CA kept for raft
  alone, or `raft.allow_insecure_transport: true`, which keeps plain TCP as
  before. A node on TLS and a node on plain TCP cannot reach each other, so a
  cluster works only while a majority of its nodes runs the same transport.
- `chmod -R go-rwx` on `data_dir`. A node refuses a `data_dir` or `snapshots`
  directory that is still open to the group or others; it makes the snapshots
  in it private by itself at its next start.

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

Retention is enforced from counters of the archive's rows, bytes and oldest
capture time, not by reading the archive: the collector scans it only when a
limit is crossed. A node that takes leadership counts it again once, since
another node may have written to it. A row ages from the time it was captured.

## Event stream

```yaml
events:
  coalesce: 1s    # env SOLTI_EVENTS_COALESCE
```

`events.coalesce` (default 1s) is the window in which the event hub coalesces the refresh pulses it sends to the
browsers' event streams (`agent_update`, `spec_update`, `dashboard_update`, `user_update`, `session_update`): per kind,
the first pulse after a quiet period goes out at once and the pulses that follow within the window become one trailing
delivery at its end, so a burst reaches a browser as two pulses, and its last change is never lost. It is also the
longest a pulse can wait. Zero keeps the default; a negative value fails configuration loading. The keepalive and
the access re-check of the stream are not pulses and are not delayed. In raft mode each replica coalesces for its own
browsers. See `internal/event/README.md`.

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
sooner. Silence counts from the later of the agent's last sync and the control
plane's start, so after a restart every agent that has not synced yet gets the
full thresholds to sync again before its status changes. Zero or negative
values inherit the defaults shown; `delete_after` is also read from
`SOLTI_LIFECYCLE_DELETEAFTER`. See `internal/server/README.md` for the states
and the restart grace.

Deleting an agent drops its row and the labels operators set on it. Its
rollouts and its credential stay: until the agent syncs again, its pushes fail
with `agent not found` until `max_retries`, and its synced rollouts are
skipped. When it syncs again, with its stored token, it comes back without
labels, so the next Deploy of a spec that targets it by labels turns its
rollout into an uninstall unless the labels are restored first.

## Rollout sync

```yaml
sync:
  tick_interval: 10s
  push_timeout: 15s    # budget of each agent call, and of an agent's turn per tick
  max_concurrency: 4   # agents worked on at once
  max_retries: 5       # failed pushes before a rollout waits for its agent
```

A failed rollout is pushed again after `tick_interval` × 2^(attempts−1), at
most 5 minutes; after `max_retries` failures it waits until its agent
reconnects or the spec is deployed again. An agent that leaves a call
unanswered holds one of the `max_concurrency` slots for one `push_timeout`,
then gets no call until its own backoff passes: the same schedule, counted in
unanswered calls and kept in memory. The backoff ends sooner when the agent row
shows a reconnect: the agent active again after it was inactive or
disconnected, or with a lower uptime. An agent that stays active keeps it. Each
agent's turn in a tick starts calls for one `push_timeout` of wall time at
most; what is left waits for the next tick, so an agent that answers slowly
holds a tick for about one `push_timeout`, not one per rollout. The check that
an agent still has its tasks is one listing of them, up to 1000 to a page (fewer
when the agent's answer would be too large), and only a whole listing counts:
an agent whose last page cannot start inside what is left of one `push_timeout`
after its pushes, or whose listing passes the bounds in the server README (65536
task IDs, 32 MiB of them, 1024 calls), is not checked. Zero or negative values
inherit the defaults shown. See `internal/server/README.md` for the scheduling.
