# podium

podium is the control plane of solti. Agents built with the
[solti SDK](https://github.com/soltiHQ/sdk) register with it and report what
they can run; operators use its web UI and REST API to write task specs, deploy
them to the agents they target, and follow the tasks and their runs. A podium
node runs standalone, on a durable store in one file, or as a node of a
[hashicorp/raft](https://github.com/hashicorp/raft) cluster.

This is the operator's guide: install, configure, run, observe, back up,
upgrade, secure, develop and release. Each section is short and links to the
package README that holds the full rules.

## How it works

```text
browsers, API clients  --->  :8080   web UI and REST API  \
agents over HTTP       --->  :8082   discovery             \
agents over gRPC       --->  :50051  discovery              >--  podium  --->  each agent's endpoint
probes, Prometheus     --->  :8081   ops                   /       |           (gRPC or HTTP)
                                                                   |
                                    state: podium.db (standalone) or a raft cluster
```

- **Discovery.** Each agent syncs with podium over HTTP
  (`POST /api/v1/discovery/sync` on the discovery listener) or gRPC
  (`solti.discover.v1.DiscoverService/Sync`). A sync is the agent's heartbeat:
  it carries the agent's endpoint, its runners with their labels and the kinds
  of workload they support, and its token. The first sync of an agent ID enrolls
  it. See [Discovery endpoint](internal/handler/README.md#discovery-endpoint).
- **Agent authentication.** With `agent_auth.require`, the default, every later
  sync of an enrolled agent must present its token, and podium presents the same
  token when it calls the agent. Closing the enrollment of new agent IDs is up
  to you: see [Provision agents](#provision-agents).
- **Specs and rollouts.** A task spec targets agents by ID and by label.
  Deploying it turns its placement into rollouts: an install or an update for
  each agent it targets, an uninstall for each agent it no longer targets. The
  sync runner pushes them to the agents' endpoints, retries failures with a
  backoff, and checks that the tasks it installed still exist on the agents. See
  [Sync runner](internal/server/README.md#sync-runner).
- **Lifecycle.** An agent that stops syncing turns inactive, then disconnected,
  and is deleted after 24 hours of silence at the earliest; at its next sync it
  comes back, without the labels operators had set on it. See
  [Lifecycle runner](internal/server/README.md#lifecycle-runner).
- **History.** The history collector copies the runs that agents report as
  finished into an archive, bounded by default to 7 days, 10000 records and 32
  MiB of record payload.
- **Users.** People log in to the web UI with a password; roles and permissions
  decide what each of them may read and change. The first start creates the
  administrator `admin`.

`cluster.backend: standalone`, the default, keeps the state in one bbolt file,
`podium.db`, in `cluster.standalone.data_dir`. `cluster.backend: raft` runs the
node as a member of a raft cluster: every node holds the whole state and serves
reads from its own copy, and a follower forwards the writes it gets to the
leader. See [Raft cluster](#raft-cluster).

## Install

Releases and images are published by the release workflow when a tag is pushed,
and it has not run yet: nothing is published before the first pushed tag, as the
note in [Release](#release) says. Until then, build a binary with
`task ci/golang/build GOOS=linux GOARCH=<arch>` (see [Develop](#develop)):
without `GOOS` and `GOARCH` it builds linux/amd64, so arm64 needs
`task ci/golang/build GOOS=linux GOARCH=arm64`. Then build an image from
`bin/podium-linux-<arch>` with
`docker build --platform linux/<arch> -t podium:local .`.

### Release binary

Each GitHub release carries `podium-linux-amd64`, `podium-linux-arm64`,
`podium-darwin-arm64` and `sha256sums.txt`. Check the binary against the
checksums, then make it executable: the release binaries are not executable as
downloaded.

```sh
VERSION=vX.Y.Z              # the release to install
ASSET=podium-linux-amd64    # or podium-linux-arm64, podium-darwin-arm64
BASE=https://github.com/soltiHQ/podium/releases/download/$VERSION
curl -fLO "$BASE/$ASSET"
curl -fLO "$BASE/sha256sums.txt"
grep " $ASSET\$" sha256sums.txt | sha256sum -c -    # on macOS: shasum -a 256 -c
chmod +x "$ASSET"
sudo mv "$ASSET" /usr/local/bin/podium
podium version
```

The binary embeds the web UI. Unless configured otherwise it keeps its state in
`data/podium` under its working directory, so a service sets an absolute
`cluster.standalone.data_dir` (see [Data directory](#data-directory)).

### Container image

`ghcr.io/soltihq/podium:vX.Y.Z` is built for linux/amd64 and linux/arm64, and
each release without a pre-release suffix also moves
`ghcr.io/soltihq/podium:latest` to its image. The image is distroless, without a
shell. It runs `/podium` as uid 65532, sets
`SOLTI_CLUSTER_STANDALONE_DATA_DIR=/var/lib/podium`, which is a volume, and
`SOLTI_OPS_ADDR=:8081`, and exposes 8080, 8081, 8082 and 50051.

```sh
umask 077
printf 'SOLTI_AUTH_JWT_SECRET=%s\n' "$(openssl rand -hex 32)" > podium.env
printf 'SOLTI_AGENT_AUTH_ENROLL_SECRET=%s\n' "$(openssl rand -hex 32)" >> podium.env

docker run -d --name podium --env-file podium.env --stop-timeout 60 \
  -p 8080:8080 -p 8082:8082 -p 50051:50051 -p 127.0.0.1:8081:8081 \
  -v podium-data:/var/lib/podium \
  ghcr.io/soltihq/podium:vX.Y.Z
```

Keep `podium.env`: a changed enrollment secret locks out every agent enrolled
under the old one, and a changed JWT secret ends every access token and device
cookie signed with the old one. `--stop-timeout 60` gives podium 60 seconds from
SIGTERM to SIGKILL: a graceful stop takes up to `server.shutdown_delay` (0s by
default), plus `server.shutdown_timeout` (15s), plus the 30 seconds in which the
hubs, the agent connections and the store close (see [Stop](#stop)): 45 seconds
with the defaults and 50 with a delay of 5s, where Docker's default of 10
seconds, and compose's, would kill it in the middle of the shutdown. Raise it
with the delay or the timeout; in compose it is `stop_grace_period`. The ops
port is published on the host's loopback only, as its routes need no
credentials: `docker run -P`, or a compose `ports:` entry such as `8081:8081`,
publishes it on every interface of the host, and with it the unauthenticated ops
listener. The image's `/var/lib/podium` belongs to uid 65532 with mode 0700, and
a new named volume such as `podium-data` starts as a copy of it. A bind mount
must be a directory that uid 65532 owns, with mode 0700. Every setting can go in
the container's environment; a configuration file passed with `--config` must be
readable by uid 65532.

In the image, `SOLTI_OPS_ADDR` (`:8081`) and `SOLTI_CLUSTER_STANDALONE_DATA_DIR`
(`/var/lib/podium`) come from the image's environment, and the environment wins
over a file: the `ops.addr` and `cluster.standalone.data_dir` of a configuration
file have no effect there. Set the variables to change them:
`-e SOLTI_OPS_ADDR=127.0.0.1:8081`, or `-e SOLTI_OPS_ADDR=` (empty) to turn the
ops listener off, and `-e SOLTI_CLUSTER_STANDALONE_DATA_DIR=...`, which must
name a directory that uid 65532 owns with mode 0700, as the image's
`/var/lib/podium` is, or a missing one that podium can create (see
[Data directory](#data-directory)). Under host networking (`--network host`, or
Kubernetes `hostNetwork`), `:8081` binds every interface of the host. podium
logs one INFO line for each setting of the file that a variable overrides, with
the setting's key and the variable.

### Data directory

podium keeps all its state in its data directory: `cluster.standalone.data_dir`
in standalone, `data/podium` under the working directory by default, and
`cluster.raft.data_dir` in raft mode. It makes a missing directory with mode
0700, and refuses to start on one that its group or others may access, or that
is a symlink: volume settings that add group permissions keep podium from
starting. Create the `/var/lib/podium` of the minimal configuration below before
the first start, mode 0700 and owned by the user that runs podium: podium makes
a missing directory only where its user may create one. The files in it are
0600: `podium.db` in standalone; in raft mode the raft log and stable store,
`raft-log.bolt` and `raft-stable.bolt`, next to a `snapshots` directory, 0700. A
generated administrator password stays in `bootstrap-admin-password`, also 0600,
until you delete it. Keep the directory across restarts and give each node its
own: never share a standalone directory between replicas, nor reuse it as a raft
`data_dir`. A lock on `podium.db` keeps a second process off it. See
[Durable standalone storage](internal/storage/durable/README.md) and
[Raft data directory](internal/config/README.md#raft-data-directory).

For development only, `cluster.standalone.ephemeral: true` keeps the state in
memory instead, and loses it at exit.

## Configure

podium reads its settings from its defaults, then a YAML file, then the
environment, which wins: each setting of the file that a variable overrides is
logged once at INFO, with its key and the variable, never a value. The file is
`--config PATH`, or `CONFIG_PATH` when that is empty; without either, podium
runs on its defaults and the environment. The variable of a setting is `SOLTI_`
and its YAML path in upper case, joined by `_`: `http.addr` is
`SOLTI_HTTP_ADDR`, `cluster.raft.tls.ca_file` is
`SOLTI_CLUSTER_RAFT_TLS_CA_FILE`. Apart from these and `CONFIG_PATH`, the
configuration reads no variable: a container's `HOSTNAME`, `PORT` or `DATA_DIR`
changes nothing.

Loading is strict. A YAML key that is not a setting stops podium at start,
before any listener or store opens, with the file, the line and the keys known
there. So do a setting written without a value (`addr:`), which would otherwise
keep its default, and a second YAML document, whose settings podium would not
read. So does a mistyped variable under a section's prefix, such as
`SOLTI_HTTP_ADRR`, with the closest setting's variable when one is close. A
`SOLTI_` variable outside every section's prefix, such as the SDK's
`SOLTI_CONTROL_PLANE`, is logged at WARN and ignored.
[Every setting](internal/config/README.md#every-setting) lists each setting with
its variable and its default.

### Required environment

| Variable | Needed | What it is |
| -- | -- | -- |
| `SOLTI_AUTH_JWT_SECRET` | always | `auth.jwt_secret`: podium refuses to start without one of 32 bytes or more. It signs the access tokens and the device cookies. |
| `SOLTI_AGENT_AUTH_ENROLL_SECRET` | to close agent enrollment | 32 bytes or more, read from the environment only. Without it, and without client certificates, any host that reaches a discovery listener can enroll a new agent ID, and podium logs a WARN at start: see [Provision agents](#provision-agents). Set while `agent_auth.require` is `false`, where it would close nothing, it fails loading. |
| `SOLTI_BOOTSTRAP_ADMIN_PASSWORD` | optional, first start | The first administrator's password, read from the environment only, 12 characters to 72 bytes; ignored once users exist. |

Generate the two secrets with `openssl rand -hex 32` and keep them: every node
of a raft cluster needs the same two values.

### A minimal standalone configuration

```yaml
# podium.yaml
cluster:
  backend: standalone            # the default
  standalone:
    data_dir: /var/lib/podium    # default data/podium, under the working directory
ops:
  addr: 127.0.0.1:8081           # the default; "" turns the ops listener off
log:
  level: info                    # debug, info, warn or error
  format: json                   # or text
```

```sh
set -a; . ./podium.env; set +a   # SOLTI_AUTH_JWT_SECRET and SOLTI_AGENT_AUTH_ENROLL_SECRET
podium --config podium.yaml
```

### Settings to look at

| Setting | Default | Notes |
| -- | -- | -- |
| `cluster.backend` | `standalone` | `raft` for a cluster: see [Raft cluster](#raft-cluster). |
| `cluster.standalone.data_dir` | `data/podium` | The standalone state: see [Data directory](#data-directory). |
| `tls.server.cert_file`, `tls.server.key_file`, `tls.server.client_ca_file` | empty | TLS on 8080, 8082 and 50051, and client certificates for agents: see [TLS](#tls). |
| `trusted_proxies` | empty | The reverse proxies whose `X-Forwarded-For` and `X-Forwarded-Proto` podium believes. |
| `auth.cookie_secure` | `auto` | `always` when the proxy that terminates TLS cannot be listed in `trusted_proxies`. |
| `auth.rotate_refresh` | `false` | In raft mode only behind leader-sticky routing: see [Raft cluster](#raft-cluster). |
| `server.shutdown_delay` | `0s` | `5s` behind a load balancer or in Kubernetes: see [Stop](#stop). |
| `server.shutdown_timeout` | `15s` | How long requests in flight get to finish at shutdown. |
| `log.level`, `log.format` | `info`, `json` | See [Logs](#logs). |

### Command line

```text
podium [--config PATH]                                runs the server
podium agent-token [--generation N] [--] <agent-id>   prints an agent's token, derived from SOLTI_AGENT_AUTH_ENROLL_SECRET
podium version                                        prints the version, as podium --version does
```

The command line is parsed before anything starts: an unknown flag or subcommand
prints the usage to stderr and exits 2. A configuration that fails to load exits
1\. See [Command line](internal/config/README.md#command-line).

## Ports

| Port | Setting | Default | Serves |
| -- | -- | -- | -- |
| 8080 | `http.addr` | `:8080` | The web UI and the REST API. |
| 8081 | `ops.addr` | `127.0.0.1:8081`, in the image `:8081` | Probes, `/version` and `/metrics`: plain HTTP, without authentication. In the image `SOLTI_OPS_ADDR=:8081` overrides a file's `ops.addr`: see [Container image](#container-image). |
| 8082 | `http_discovery.addr` | `:8082` | Agents' HTTP discovery, `POST /api/v1/discovery/sync`. |
| 50051 | `grpc.addr` | `:50051` | Agents' gRPC discovery, `solti.discover.v1.DiscoverService`, and `grpc.health.v1.Health`. |
| raft | `cluster.raft.bind_addr` | none: required in raft mode, 7000 in the examples | The raft transport between the nodes: mutual TLS 1.3, or plain TCP with `cluster.raft.allow_insecure_transport`. |

`tls.server` puts TLS on 8080, 8082 and 50051; the ops listener always speaks
plain HTTP. Every node of a raft cluster serves `http.addr`,
`http_discovery.addr` and `grpc.addr` on the same ports as the others, as a
follower forwards a write to the leader's host at its own port of the listener
the write came in on.

## Run

### First start

On the first start, while there is no user, podium creates the administrator
`admin`. Its password is `SOLTI_BOOTSTRAP_ADMIN_PASSWORD` when that is set,
which podium writes to no file and no log. Otherwise podium generates one and
writes it to `bootstrap-admin-password` in the data directory, mode 0600, and
its log names the file. From the container above:

```sh
docker cp podium:/var/lib/podium/bootstrap-admin-password - | tar -xO
```

Log in on port 8080, change the password, and delete the file. Remove
`SOLTI_BOOTSTRAP_ADMIN_PASSWORD` once users exist: podium ignores it then, and
warns about it at each start, or at each leadership term of a raft node. In raft
mode the file is on the node whose leadership term seeded the administrator, the
one that logged `admin password generated`. In ephemeral mode the generated
password is logged once, at WARN. See
[First administrator](internal/config/README.md#first-administrator) and
[First-start bootstrap](internal/bootstrap/README.md).

### Stop

The first SIGTERM or SIGINT starts a graceful shutdown:

1. `/readyz` turns to 503 `shutting down`, and the gRPC health service to
   `NOT_SERVING`, at once; every listener still serves.
1. podium waits `server.shutdown_delay`, 0s by default: the time a load balancer
   needs to see the 503. Behind a load balancer or in Kubernetes, set `5s`.
1. The listeners stop accepting. The requests in flight finish within
   `server.shutdown_timeout`, 15s by default, and a request still running then
   is cut; the event and log streams end.
1. The hubs, the agent connections and the store or the raft node close, within
   30 seconds, which is not configurable.

A termination grace period must cover the delay, the timeout and those 30
seconds. A second signal during the shutdown ends the process at once, with
status 1. podium exits 0 after a clean shutdown, and 1 when a listener stops on
its own, one that cannot bind its port for instance. See
[Shutdown](internal/config/README.md#shutdown).

## Provision agents

With `SOLTI_AGENT_AUTH_ENROLL_SECRET` set, the first sync of an agent ID enrolls
it only with the token podium derives from the secret for that ID; any other
token is refused with 401 or `Unauthenticated`. Derive the token on a host that
has the secret, and give it to the agent, which must run with the same ID, 1 to
128 bytes of `[A-Za-z0-9._-]`:

```sh
export SOLTI_AGENT_AUTH_ENROLL_SECRET=...   # the value podium runs with
podium agent-token agent-1                   # prints the token and a newline
```

With the image:
`docker run --rm --env-file podium.env ghcr.io/soltihq/podium:vX.Y.Z agent-token agent-1`.
The agent passes the token to the SDK, with `with_token(Token::new(token)?)` or
`Token::from_env(...)`, and the SDK sends it only over TLS unless the agent
allows otherwise with `allow_insecure_token_transport()`. Agents never see the
secret: a leaked token lets nobody enroll another ID. See
[Enrollment secret](internal/config/README.md#enrollment-secret).

- **Without an enrollment secret**, enrollment is trust on first use: the first
  sync of an unknown agent ID enrolls the token it presents and sets the
  endpoint podium calls, and podium logs a WARN at start that says so. Close
  enrollment with the secret, with client certificates
  (`tls.server.client_ca_file` makes the discovery listeners require a
  certificate of the agents' CA), or keep the discovery listeners reachable from
  agent networks only. See
  [Agent authentication](internal/config/README.md#agent-authentication).
- **`agent_auth.require: false`** turns agent tokens off in both directions, and
  podium logs a WARN at start. Configuration loading refuses it while
  `SOLTI_AGENT_AUTH_ENROLL_SECRET` is set, as in the `podium.env` above: unset
  the secret first.
- **A leaked or lost token**: reset the agent's credential with
  `POST /api/v1/agents/{id}/credential`, which needs `agents:reset`, or "Reset
  credential" on the agent's page. The answer holds the new token to give the
  agent, the only place it appears, and the old token stops working at once in
  both directions. See
  [Resetting a credential](internal/config/README.md#resetting-a-credential).
- **A changed or removed enrollment secret** locks out every agent enrolled
  under the old one:
  [Enrollment secret](internal/config/README.md#enrollment-secret) gives the
  ways back, re-provisioning each agent or resetting its credential among them.
- **Deleting an agent** (`DELETE /api/v1/agents/{id}`) keeps its credential, so
  an agent that still runs registers again at its next sync: reset its
  credential first to keep it out.
- **Sync rate.** Each agent ID may sync 10 times in a burst and then once a
  second; beyond that the sync is answered HTTP 429 or `ResourceExhausted`,
  which the SDK retries. See [Sync rate](internal/config/README.md#sync-rate).

podium calls each agent at the endpoint its sync registered, a gRPC `host:port`
or an `http(s)://` URL, so the agents' endpoints must be reachable from podium,
from every node of a raft cluster. See
[Endpoints and credentials](internal/proxy/README.md#endpoints-and-credentials).

## TLS

Both halves are opt-in:

- `tls.server` (`cert_file`, `key_file`) is podium's certificate on 8080, 8082
  and 50051. With `client_ca_file` set, the discovery listeners, 8082 and 50051,
  require a client certificate of that CA; the UI and API listener never asks
  for one, so browsers keep working.
- `tls.client` is podium as a client of the agents: `ca_file` verifies their
  certificates, and `cert_file` with `key_file` is the certificate podium
  presents when an agent asks for one. With an empty `tls.client`, podium dials
  an agent's gRPC or `http://` endpoint in plaintext, and an `https://` endpoint
  over TLS against the system roots.

```yaml
tls:
  server:
    cert_file: /etc/solti/tls/server.crt
    key_file:  /etc/solti/tls/server.key
    client_ca_file: /etc/solti/tls/agents-ca.crt   # optional: agents need a client certificate
  client:
    ca_file:   /etc/solti/tls/client-ca.crt        # verifies agents' endpoints; raft: also the nodes' server certificates
    cert_file: /etc/solti/tls/cp-client.crt        # presented when asked; raft with client_ca_file: required, of the agents' CA
    key_file:  /etc/solti/tls/cp-client.key
```

In a raft cluster a follower forwards writes and agents' syncs to the leader,
over TLS as a client of `tls.client` once `tls.server` is set.
`tls.client.ca_file` must then also hold the CA of the nodes' server
certificates, which name each node's raft advertise host as written (without a
`ca_file` the leader is checked against the system roots): a leader whose
certificate does not verify gets no write, and the follower answers 502 over
HTTP and `leader unreachable` over gRPC. With `client_ca_file` set too, the
leader's discovery listeners ask a forwarding follower for a certificate of the
agents' CA, so each node's `tls.client.cert_file` and `key_file` must chain to
that CA: without `tls.client.cert_file`, every sync that reaches a follower
fails, 502 over HTTP and `leader unreachable` over gRPC, and each node logs a
WARN at start.

podium reads its certificate files at start only, so a renewed certificate takes
effect at the next start. Behind a reverse proxy that terminates TLS, list the
proxy in `trusted_proxies`, so that podium sees the client's IP and that it used
HTTPS; set `auth.cookie_secure: always` if the proxy cannot be listed. A proxy
in front of the web UI keeps its read timeout above 15 seconds: a quiet event or
log stream sends a keepalive only after 15 seconds. A raft cluster also needs
`tls.server` on every node, and `cluster.raft.tls` with a CA of its own: see
[Raft cluster](#raft-cluster). See [TLS](internal/config/README.md#tls) and
[Client IPs and trusted proxies](internal/config/README.md#client-ips-and-trusted-proxies).

## Back up and restore

### Standalone

A backup is a cold copy of the data directory: stop podium cleanly and copy the
directory, whose state is `podium.db`. To restore, stop podium and put the copy
back as its data directory, the directory 0700 and `podium.db` 0600, owned by
the user podium runs as, uid 65532 in the image. Never copy `podium.db` while
podium runs, and never delete it to get past a failed start: keep the failing
file and restore a verified backup. See
[Durable standalone storage](internal/storage/durable/README.md#log-and-snapshots).

```sh
# podium is stopped; its data_dir is /var/lib/podium
cp -a /var/lib/podium "/var/backups/podium-$(date +%F)"
```

### Raft

Each node keeps the whole state in its own `cluster.raft.data_dir`: the raft log
and the stable store, which are bbolt files like `podium.db`, and the snapshots.
Copy a node's `data_dir` only while that node is stopped: as with `podium.db`, a
live bbolt file is never copied. Copies of the nodes taken at different times
are not one consistent point to roll back to. Rolling back an upgrade needs a
copy of every node's `data_dir` taken before the upgrade
([Rolling upgrade](internal/raft/README.md#rolling-upgrade)), and no procedure
to restore a cluster from such copies is documented. A node whose `data_dir` was
lost or wiped rejoins as a new node: remove it from the cluster, start it with
an empty `data_dir` and `cluster.raft.join: true`, and add it again, as
[Adding and removing members](internal/raft/README.md#adding-and-removing-members)
says.

No test in this repository restores a copied data directory: try your backups on
a disposable deployment.

### What a backup holds

- **All of podium's state, unencrypted**, including the users with their
  password hashes (bcrypt), the roles, the sessions (refresh tokens as SHA3-256
  hashes, session IDs as they are), the agents and their credentials, the task
  specs as they were written, with whatever an operator put in a task's command
  or config, credentials included, the rollouts, the run history, which holds no
  command, environment or log, and `bootstrap-admin-password` while it exists.
- **Agent credentials.** An agent enrolled under the enrollment secret has a
  derived credential: a generation and an ID of the secret, and no token. An
  agent that enrolled without the secret, or that a version before derived
  credentials enrolled, has a stored credential: its token, raw.
- **Tokens from before derived credentials.** Upgrading to the version with
  derived credentials leaves the raw tokens already written in the snapshot and
  log entries until the store compacts them, and in the file's freed pages until
  bbolt writes over them. Take a fresh backup once the store has compacted past
  the upgrade, and drop the older ones.
- **Old credentials come back with an old backup.** Restoring a backup taken
  before a credential reset brings back the credential it held, and its token
  works again, until the agent's credential is reset once more.
- **Not the secrets.** `SOLTI_AUTH_JWT_SECRET` and
  `SOLTI_AGENT_AUTH_ENROLL_SECRET` are in your configuration, not in the data
  directory. Back them up too, apart from the data: a derived credential is of
  no use without the secret it derives from.

See [A compromised node](internal/raft/README.md#a-compromised-node), which
lists everything podium keeps on a node, and
[Enrollment secret](internal/config/README.md#enrollment-secret).

## Upgrade

podium has no downgrade. A binary refuses a snapshot or a log entry of a codec
version it does not know (`unsupported version`, `unsupported command version`),
and a log entry with an operation or a field it does not know, so once a newer
version has written to a data directory, an older one may refuse to start on it.
The version that brought derived agent credentials writes version 4 of the
snapshot and command codecs, which every earlier version refuses from the first
write after the upgrade. The way back is the copy taken before the upgrade,
without what was written since.

- **Standalone.** Stop podium, take a cold copy of the data directory, replace
  the binary or the image tag, and start. The first start of a version may
  migrate the file, once, and log a warning. If the new version refuses to
  start, keep the failing file; to go back, restore the copy and start the
  earlier version. See
  [Upgrading](internal/storage/durable/README.md#upgrading).
- **Raft.** Read [Rolling upgrade](internal/raft/README.md#rolling-upgrade)
  first. Upgrade one node at a time, and the next only once the last one follows
  the leader again (`GET /api/v1/cluster`), unless the new version writes a
  format the earlier one cannot read: then stop every node, upgrade them all and
  start them again, in one full stop and start, during which the cluster takes
  no write. An upgrade from an earlier version also has prerequisites:
  [Upgrading a raft node](internal/config/README.md#upgrading-a-raft-node).
- **New permissions.** A version that adds permissions gives them to the
  built-in Admin role, which bootstrap reconciles at each standalone start and
  at each raft leadership term. A user who holds every permission directly, not
  through the Admin role, does not gain them, and needs them to keep counting as
  an administrator.

## Observe

### Ops endpoints

The ops listener, `ops.addr`, serves plain HTTP, never asks for credentials, and
answers GET and HEAD with one line of text, but for `/version` and `/metrics`:

| Route | Answers |
| -- | -- |
| `/livez` | 200 `ok` while the process serves. |
| `/readyz` | 200 `ok` when the node is ready; otherwise 503 with the first reason that applies: `shutting down`, `starting`, `storage failed` or `no raft leader`. |
| `/leaderz` | 200 on the raft leader once its start-up has finished, also during the shutdown delay, and on a standalone node while it is ready; otherwise 503. |
| `/version` | The build information as JSON: `version`, `modified`, `go_version` and `platform`, and `commit` and `build_time` when the binary carries them. |
| `/metrics` | The Prometheus text exposition, below. |

Probe liveness with `/livez`, which stays 200 whatever the store does, and
readiness with `/readyz`. A load balancer that must reach the raft leader
health-checks `/leaderz`. The gRPC listener serves `grpc.health.v1.Health`
without authentication: the overall service and
`solti.discover.v1.DiscoverService` are `SERVING` while `/readyz` answers 200;
with `tls.server.client_ca_file` set, the listener asks a health checker for a
client certificate of the agents' CA too. With `ops.addr: ""` the ops listener
is off. Keep it on an internal network: it tells the node's health, its
leadership, its build and its metrics. See
[Ops listener](internal/config/README.md#ops-listener).

### Metrics

Every name starts with `podium_`, next to the Go and process collectors' series.
No label carries an agent, user, spec, task or session ID, or a token.

| Metric | Type | Labels | Meaning |
| -- | -- | -- | -- |
| `podium_build_info` | gauge | `version`, `commit`, `goversion` | Always 1. |
| `podium_http_requests_total` | counter | `server` (`ui`, `discovery`), `route`, `method`, `code` | Requests of the UI and discovery listeners; `route` is the matched route pattern, or `other`. |
| `podium_http_request_duration_seconds` | histogram | `server`, `route`, `method` | Their duration; event streams are not timed. |
| `podium_grpc_requests_total` | counter | `method`, `code` | Calls of the gRPC listener. |
| `podium_grpc_request_duration_seconds` | histogram | `method` | Their duration, unary calls only. |
| `podium_discovery_syncs_total` | counter | `transport` (`http`, `grpc`), `outcome` (`ok`, `enrolled`, `unauthenticated`, `refused`, `invalid`, `rate_limited`, `unavailable`) | How each sync this node served ended. |
| `podium_logins_total` | counter | `outcome` (`ok`, `failed`, `limited`) | Login attempts in the web UI. |
| `podium_agents` | gauge | `status` (`active`, `inactive`, `disconnected`) | Agents in this node's store, counted at scrape. |
| `podium_rollouts` | gauge | `status` (`pending`, `synced`, `drift`, `failed`, `unknown`) | Rollouts in this node's store, counted at scrape. |
| `podium_sync_turns_total` | counter | `outcome` (`answered`, `unanswered`, `local`) | Agent turns of the sync runner, which runs on the leader only. |
| `podium_sync_turn_duration_seconds` | histogram | none | Their duration. |
| `podium_store_commit_duration_seconds` | histogram | `backend` (`durable`, `raft`) | A durable commit, or a raft write from its proposal to its answer, which only the leader makes. |
| `podium_store_failed` | gauge | none | 1 while the store has failed, which `/readyz` reports as `storage failed`. |
| `podium_raft_state` | gauge | `state` (`follower`, `candidate`, `leader`, `shutdown`) | Raft nodes only: 1 for the node's state. |
| `podium_raft_term` | gauge | none | The node's current term. |
| `podium_raft_last_index` | gauge | none | The index of the last entry in its log. |
| `podium_raft_applied_index` | gauge | none | The last entry it applied; `last_index` minus it is what it has yet to apply. |
| `podium_raft_last_contact_seconds` | gauge | none | Seconds since a follower last heard from the leader; absent on a node that is not a follower. |
| `podium_raft_peers` | gauge | none | The other voting members of the node's configuration. |

See [Metrics](internal/server/README.md#metrics) and, for a raft node,
[Metrics](internal/raft/README.md#metrics).

### Logs

podium logs to stdout. `log.format` is `json`, the default, one object per line,
or `text`, one line per event without colour; `log.level` is `debug`, `info`,
the default, `warn` or `error`. Until the configuration is loaded podium logs
JSON at `info`: the first line of a run, `podium starting`, with the version,
the commit and the platform, and what loading reports. The raft library's lines
follow the same settings. Libraries that log through their own default logger,
such as grpc-go's `grpclog`, write to stderr. See
[Logging](internal/config/README.md#logging) and, for a raft node,
[Logging](internal/raft/README.md#logging).

## Security notes

- **Secrets.** Make `auth.jwt_secret` and the enrollment secret long random
  values. Whoever reads a raft node's `config_fingerprint`, which
  `GET /api/v1/cluster` shows behind `cluster:get`, and knows the other settings
  can confirm a guess of a weak one offline. The enrollment secret derives the
  token of every agent enrolled under it; podium reads it from the environment
  only, so no configuration file holds it.
- **The ops listener** has no authentication: keep it on loopback, the default,
  or on an internal network.
- **Discovery** without an enrollment secret or client certificates lets any
  host that reaches 8082 or 50051 enroll new agent IDs: see
  [Provision agents](#provision-agents).
- **The state is not encrypted.** The data directory and its backups hold the
  password hashes, the raw tokens of stored agent credentials, the session IDs
  and the task specs as written: keep credentials out of tasks' commands and
  configs, or treat the data directory and its backups as secrets.
- **Agent tokens on the wire.** podium presents an agent's token on every call
  to it, so an agent endpoint without TLS carries the token in plaintext.
- **Browsers.** The main listener refuses state-changing requests that a browser
  sends from another origin, with 403, and CORS grants no other origin by
  default. The session cookies are HttpOnly, and `Secure` as
  `auth.cookie_secure` says. See
  [Browser sessions](internal/config/README.md#browser-sessions).
- **Logins.** Passwords are stored as bcrypt hashes, cost 12, and a new one
  needs 12 characters to 72 bytes. Five failed logins, each within a minute of
  the last, block the account, and the client IP, for a minute
  (`auth.rate_attempts`, `auth.rate_window`); a browser that logged in as the
  account within 90 days keeps a counter of its own, so wrong passwords from
  elsewhere do not lock it out. A restart clears the limiters. Behind a reverse
  proxy, list it in `trusted_proxies`, or every client shares the proxy's IP.
  See [Rate limiting](internal/config/README.md#rate-limiting) and
  [Security model](internal/auth/README.md#security-model).
- **Raft.** The raft port carries the whole state: give it mutual TLS with a CA
  that signs raft node certificates only, of one cluster, and let only the
  cluster's nodes reach it. Removing a node revokes nothing, as podium checks no
  certificate revocation: after removing a node whose keys may be exposed, issue
  a new raft CA.
  [A compromised node](internal/raft/README.md#a-compromised-node) lists
  everything a node keeps, with each rotation and its cost.

## Raft cluster

To run a cluster, follow
[Runbook: a three-node cluster](internal/raft/README.md#runbook-a-three-node-cluster):
certificates, network, configuration, the first start, restarts and failover,
adding and removing members, a compromised node, browsers and
`auth.rotate_refresh`, and rolling upgrades. In short:

- Each node has its own `cluster.raft.node_id` and `data_dir`, and
  `cluster.discovery.peers` lists every node, itself included. Prefer static
  peers for the first start: with the dns driver a node that resolves fewer
  peers bootstraps a smaller cluster, and nothing reports it.
- The raft transport runs mutual TLS 1.3 (`cluster.raft.tls`), with a CA kept
  for raft alone, never the agents' CA or the CA of `tls.server` or
  `tls.client`. See
  [Raft transport security](internal/config/README.md#raft-transport-security).
- Set `tls.server` on every node, with certificates that name each node's raft
  advertise host as written, and put their CA in every node's
  `tls.client.ca_file`: a follower forwards the writes it gets, logins, cookies
  and tokens included, and the agents' syncs to the leader, in plaintext without
  `tls.server`, and each node logs a WARN at start that says so. With
  `tls.server.client_ca_file`, give every node a `tls.client.cert_file` and
  `key_file` that chain to the agents' CA, or every sync that reaches a follower
  fails: see [TLS](#tls).
- Some settings must be the same on every node: `auth.jwt_secret`,
  `auth.cookie_secure`, `auth.issuer`, `auth.audience`, `agent_auth.require`,
  the enrollment secret, `trusted_proxies`, `cors.*` and
  `tls.server.client_ca_file`. Each node compares them with its leader's and
  logs an ERROR when they differ. See
  [Settings every replica must share](internal/config/README.md#settings-every-replica-must-share).
- Every node serves reads from its own store, which can lag the leader's,
  revocations included: see
  [Reads on a follower](internal/raft/README.md#reads-on-a-follower). `/readyz`
  answers 503 `no raft leader` while a node knows no leader.
- With `auth.rotate_refresh: true`, a read from a browser whose access token has
  expired gets 503 on a follower until a request reaches the leader, so routing
  must be leader-sticky, by `/leaderz`; keep rotation off otherwise. See
  [Browsers and `auth.rotate_refresh`](internal/raft/README.md#browsers-and-authrotate_refresh).
- `GET /api/v1/cluster` (`cluster:get`) shows a node's role, its leader, the
  members, its `last_index` and its `config_fingerprint`;
  `POST /api/v1/cluster/members` and `DELETE /api/v1/cluster/members/{id}`
  (`cluster:manage`) change the membership. See
  [Cluster](internal/handler/README.md#cluster-apiv1cluster).

## Develop

Every task runs through [Task](https://taskfile.dev), and the build and check
tasks run in the CI containers of `ghcr.io/mr-chelyshkin/ci`: you need Task,
Docker and git, and no local Go or Node. The Taskfile includes the shared task
files of `github.com/mr-chelyshkin/tasks`, pinned by commit. `task --list` shows
the tasks.

| Task | Does |
| -- | -- |
| `task prepare` | Generates the sources and builds the UI assets, for a local run. |
| `task generate` | Vendors the proto schemas of `soltiHQ/proto` at `proto_ref`, and generates the Go bindings and the templ components. |
| `task assets` | Downloads Alpine and HTMX, each checked against the sha256 pinned in `Taskfile.yml`, and builds the Tailwind CSS. |
| `task ci/golang/build GOOS=linux GOARCH=amd64` | Builds `bin/podium-<os>-<arch>` with the UI embedded; linux/amd64 by default. |
| `task ci/golang/fmt`, `task golang/fix` | Checks, or applies, gofumpt. |
| `task ci/golang/lint` | Runs golangci-lint. |
| `task ci/golang/test` | Runs every test with the race detector. |
| `task ci/golang/audit` | Runs govulncheck. |
| `task ci/golang/bench` | Runs every benchmark once, as a smoke check, not a performance gate. |
| `task ci/ui/test` | Runs the UI's Node tests. |
| `task ci/markdown/fmt`, `task markdown/fix` | Checks, or applies, mdformat on every README. |

To try a build, run it with its state in memory, which logs the generated
administrator password at WARN:

```sh
task ci/golang/build GOOS=darwin GOARCH=arm64
SOLTI_AUTH_JWT_SECRET=$(openssl rand -hex 32) SOLTI_CLUSTER_STANDALONE_EPHEMERAL=true ./bin/podium-darwin-arm64
```

Run one package's tests with
`task golang:exec CMD='go test -race ./internal/config/'`, once `task generate`
has run. Pull requests to `main` run the Markdown check, the UI tests, the Go
checks (fmt, lint, test, audit, bench) and the three builds, and need one
changelog label: `feature`, `bug`, `docs`, `deps`, `tooling`, `other` or `skip`.

## Release

1. Tag a commit of `main` with `vX.Y.Z`, or `vX.Y.Z-<pre-release>` such as
   `v1.2.0-rc.1`: SemVer with a `v` prefix, without `+build` metadata, which an
   image tag cannot carry, and with a major version of 0 or 1. Go gives v2 and
   later versions only to a module path that ends in `/v2`, which
   `github.com/soltiHQ/control-plane` does not, so a binary built at a v2 tag
   would not report it: verify refuses such a tag, and v2 needs the module path
   changed first. Push the tag:

   ```sh
   git tag vX.Y.Z <commit>
   git push origin vX.Y.Z
   ```

1. The release workflow, `.github/workflows/release.yml`, runs on the pushed
   tag. Its runs do not overlap: they share one concurrency group, without
   `cancel-in-progress`, and GitHub keeps one run waiting behind the one in
   progress. A third tag pushed while one run is in progress and another waits
   cancels the waiting run, whose tag is then not released. Push one release tag
   at a time.

   - **verify** checks the tag's format, and that `main` contains its commit.
   - **build** builds `podium-linux-amd64`, `podium-linux-arm64` and
     `podium-darwin-arm64` with `task ci/golang/build`, which embeds the checked
     UI assets.
   - **image** builds the image for linux/amd64 and linux/arm64 from the linux
     binaries, without emulation, as no stage of the Dockerfile runs a program
     of the target platform, on a BuildKit image pinned by digest, and pushes
     `ghcr.io/soltihq/podium:vX.Y.Z`, with OCI labels. Then, for a tag without a
     pre-release suffix, it points `ghcr.io/soltihq/podium:latest` at the same
     digest. No job starts the image: it is pushed without a run of it.
   - **release** creates the GitHub release with generated notes, the three
     binaries and `sha256sums.txt`, marked as a pre-release for a tag with a
     pre-release suffix. The notes group the pull requests by their changelog
     label, and leave out those labelled `skip`.

1. A failed verify or build publishes nothing. An image job that fails after the
   push leaves `ghcr.io/soltihq/podium:vX.Y.Z` pushed, with `:latest` where it
   was, and no GitHub release is made. A failed release job, after the image job
   passed, leaves `ghcr.io/soltihq/podium:vX.Y.Z` pushed, and `:latest` moved to
   it for a tag without a pre-release suffix, with no GitHub release.

The workflow has not run yet: its first run is the first pushed tag. Before that
tag, an owner of the soltiHQ organization must allow this repository's workflows
to publish packages with `GITHUB_TOKEN`: the image job logs in to ghcr.io with
it, with `packages: write`. After the first release, check the visibility of the
`podium` package in the organization's package settings: pulling the image
without credentials needs it public.

To update Alpine or HTMX, change the version and its sha256 in `Taskfile.yml`
together: see [Verification](ui/README.md#verification).

## Documentation

| README | Covers |
| -- | -- |
| [internal/config](internal/config/README.md) | Every setting and its variable, the command line, logging, the ops listener, shutdown, TLS, agent authentication and enrollment, trusted proxies, browser sessions, rate limits, storage and cluster settings, and upgrading a raft node. |
| [internal/raft](internal/raft/README.md) | The runbook of a three-node cluster, transport security, membership, configuration drift, follower reads and writes, and a raft node's metrics and logs. |
| [internal/storage/durable](internal/storage/durable/README.md) | The standalone store: its file, upgrades, crash safety, backups and performance. |
| [internal/bootstrap](internal/bootstrap/README.md) | The built-in roles and the first administrator. |
| [internal/auth](internal/auth/README.md) | Logins, sessions, passwords, login rate limiting and device cookies. |
| [internal/handler](internal/handler/README.md) | The REST API's routes and permissions, discovery, and the cluster API. |
| [internal/server](internal/server/README.md) | The runners, the metrics, and the lifecycle and sync runners. |
| [internal/proxy](internal/proxy/README.md) | How podium calls the agents. |
| [internal/transport](internal/transport/README.md) | The HTTP and gRPC request path, client IPs, the leader hint and the TLS wiring. |
| [ui](ui/README.md) | The web UI and its assets. |

[internal/app](internal/app/README.md),
[internal/event](internal/event/README.md),
[internal/service](internal/service/README.md),
[internal/storage](internal/storage/README.md),
[internal/transportctx](internal/transportctx/README.md) and
[internal/uikit](internal/uikit/README.md) describe the code's structure.

## License

[Apache License 2.0](LICENSE).
