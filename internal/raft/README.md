# Package raft

Package raft runs the control-plane state on hashicorp/raft, with BoltDB log
and stable stores and a file snapshot store in `cluster.raft.data_dir`. `New`
starts a node and returns a `Profile`: `Store()` submits writes to the raft
log, which applies them on every node, and `Leadership()` reports whether this
node leads. The settings are described under "Cluster" in
`internal/config/README.md`.

## Node IDs and peers

Every node has an explicit ID, `cluster.raft.node_id`, and every node lists
every node, itself included. With the static driver each entry of
`cluster.discovery.peers` is `id=host:port`, or a bare `host:port` named by its
address; with the dns driver each address the hostname resolves to is a peer at
`ip:<discovery.port>`, named by that address. A node is listed as its
`node_id` at its advertise address: `advertise_addr`, or `bind_addr` without
it. Earlier versions appended a node to its own peers; a node upgraded from one
must list itself first, see "Upgrading a raft node" in
`internal/config/README.md`.

## Validation

`cluster.Config.Validate` checks the raft settings and the peers when the
configuration loads, and `buildCluster` runs it again before it builds
discovery or calls `New`, so a bad configuration fails before anything in the
data directory is opened (`TestRaftConfigValidatedBeforeStoreOpens`):
`node_id`, `bind_addr` and `data_dir` are required, a `bind_addr` on every
interface needs an `advertise_addr`, bare entries need `node_id` to be the
advertise address, and so does the dns driver, whose advertise address must be
the node's `ip:<discovery.port>`. A static list must also pass the checks of
`cluster.ServerList` below. A node that joins a running cluster asks discovery
for nothing, so only the syntax of its peers is checked, and the host name and
port of dns (`TestJoiningNodeValidation`, see "Joining a running cluster").

## Bootstrap

`New` bootstraps only on a node's first start, while its data directory holds
no raft state:

1. It asks discovery for the peers. A discovery error fails the start.
2. `cluster.ServerList` sorts them by ID. It fails the start when an ID or an
   address appears twice, or when no peer is this node's `NodeID` at its
   advertise address, compared as written.
3. The node writes exactly that list, every server a voter, into its stores
   with hashicorp/raft's package-level `BootstrapCluster`, then starts raft
   with `NewRaft`. The transport listens already, but nothing consumes its
   RPCs before `NewRaft`, so no vote request can persist a term first, which
   would give the node state and make the bootstrap fail with
   `ErrCantBootstrap` (`TestFirstStartBootstrapsBeforeRaftRuns`).

The list is never rewritten for the node that derives it, so every node
derives the same configuration whatever order discovery reports the peers in
(`TestAllNodesDeriveTheSameServerList`). hashicorp/raft expects that of
`BootstrapCluster`, and once it has a configuration, a node rejects the vote of
a candidate missing from it (`requestVote` in hashicorp/raft v1.8.0). Every
node must therefore see the same peers at its first start: nothing compares the
lists of different nodes, so DNS answers that differ between nodes go
unnoticed, while an address DNS answers twice fails the start as a duplicate.
Three nodes named `n1`, `n2` and `n3` elect a leader and replicate a write
(`TestThreeNodesWithNamedIDsElectLeader`).

The list is derived before the transport listens, and a start that fails on
it keeps no raft state, so the next start bootstraps again
(`TestFirstStartRefusesPeersWithoutThisNode`). DNS answers get the same checks
at start: a node that advertises a host name is not among the IP answers, so
its start fails instead of listing the node twice
(`TestBootstrapRefusesInconsistentPeers`).

Later starts load the configuration from the node's own log and do not ask
discovery: a node restarted while discovery fails rejoins its cluster
(`TestRestartDoesNotAskDiscovery`).

### Joining a running cluster

`cluster.raft.join: true` (`Config.Join`) starts a node whose data directory
holds no raft state without a bootstrap: `New` neither asks discovery nor writes
a configuration, logs one INFO line that the node waits to be added, and starts
raft with no configuration, so the node leads nothing. The leader of the running
cluster adds it through the membership API (`POST /api/v1/cluster/members`, see
"Membership" below); the leader's log, or a snapshot, then reaches the node, and
it catches up the state written before it joined
(`TestJoiningNodeWaitsToBeAdded`). Validation accepts a joining node that
`discovery.peers` does not list, and an empty list, though a list that is set
must still parse. A node with raft state loads its configuration from its own
log whatever the flag says, so a flag left set changes nothing at a restart.
Clearing it is another matter: without the flag, validation needs
`discovery.peers` to list the node as its `node_id` at its advertise address,
and with the dns driver needs its `node_id` and advertise address to be its
`ip:<discovery.port>`. So a node the API added keeps the flag set until its
`discovery.peers` lists it, and with dns until its `node_id` matches too:
cleared earlier, the flag's absence fails every start of the node at
validation. A node that replaces
another, or whose data directory was wiped, starts with the flag, so that it
does not bootstrap a configuration of its own from the static list.

## Leadership terms

`Leadership.WhenLeader` runs its function while the node leads, with a context
that the end of the term cancels, and only once a barrier (`WaitReady`) saw
every committed write applied, so the work of a term reads what every earlier
term wrote. A function that returns while the node still leads runs again,
after another barrier, so it must return only once its context ends: one that
returns early runs again and again, and each barrier is an entry in the raft
log. The background runners use it: the lifecycle, sync and history runners
tick until the term ends, and the `bootstrap` runner seeds at its start and then
waits for the term to end, see `internal/bootstrap/README.md`; nothing waits
for a leader at start. A barrier that fails while the node still leads, one
that timed out on a slow quorum for instance, is no leadership loss: one WARN,
`raft: the applied state is not ready at the start of a leadership term:
waiting again`, a pause of a second, and another barrier, as long as the term
lasts (`TestWhenLeaderRetriesAFailedWaitReady`). Only the caller's context, a
raft that shuts down, or an error of the function ends `WhenLeader`, so such a
barrier no longer ends a runner, and with it the server
(`TestWhenLeaderRetriesWaitReadyUntilItsContextEndsOrRaftCloses`). Both tests
run on one node on loopback TCP with mutual TLS, each with a `Leadership` of
its own over the node's raft, whose barrier they replace.

## Membership

`Profile.Admin` returns the node's cluster administration (`admin.go`): the
narrow `cluster.Admin` of the cluster service (`internal/service/cluster`)
behind the membership API, which never hands out the hashicorp/raft node. It
reads the node's ID, role, leader, last index and members, adds a voter, removes
a server, transfers the leadership, and probes an address. The API is in
"Cluster" of `internal/handler/README.md`.

### The configuration and its index

A change is checked against the latest configuration and applied against its
index, hashicorp/raft's `prevIndex`: a change of the configuration in between
refuses it, instead of letting it slip past the checks. hashicorp/raft v1.8.0
has no call that gives that index: `GetConfiguration` reads a copy of the
configuration that holds none, and its `Index` is always 0, which as a
`prevIndex` checks nothing. So `Admin.Configuration` reads the configuration
and its index from the node's own stores, as `NewRaft` rebuilds them at start:
the last `LogConfiguration` entry above the latest snapshot's index, decoded
with `DecodeConfiguration`, or else the snapshot's configuration and
configuration index. That is Raft's latest configuration, committed or not,
which hashicorp/raft compares a `prevIndex` with. The read runs while raft
writes, and every race leaves the index it gives behind, never ahead: a change
against a stale index changes nothing. The index is the one the change's
`IndexFuture` gave (`TestConfigurationIndexIsTheChangesIndex`), and it stays
right once a snapshot compacted the log past the configuration's entry
(`TestConfigurationIndexSurvivesCompaction`). The read scans and decodes every
entry above the latest snapshot, once per change, on the leader; the status
reads the copy. Under heavy writes that is up to `snapshot_threshold` entries
plus what `snapshot_interval` times the write rate adds between two snapshot
checks; nothing has measured what it costs on a large log. While a snapshot is
being written, the snapshot store's `List` can log hashicorp/raft's WARN `found
temporary snapshot`.

A leader's configuration is at index 1 at least, the bootstrap entry, and lists
the leader. A read that fails, or gives index 0 or no server, is refused, and
logged at ERROR as `cannot read the raft configuration`: index 0 as `prevIndex`
would turn hashicorp/raft's check off, and an empty list would let every check
pass, so that an existing voter's ID added at another address would silently
move that voter. The API answers it 500, an internal error
(`TestUnreadableConfigurationRefusesEveryChange`). That happens when no
snapshot's `meta.json` is readable, which hashicorp/raft's snapshot store skips
with a WARN, after compaction dropped the configuration's entry. When an older
snapshot's meta is still readable, the read falls back to it instead, and if
the configuration changed after that snapshot, its index is stale: every
change is then refused as `configuration changed, retry`, 409, however often it
is retried, until the node writes a new snapshot.

hashicorp/raft refuses a change against an index that is no longer the latest
with `configuration changed since %v (latest is %v)`, which has no sentinel:
`Admin` gives it as `cluster.ErrConfigurationChanged` by that text, a conflict,
which the API answers 409 `configuration changed, retry`. The mapping is tied
to v1.8.0: `TestStaleConfigurationIndexConflicts` pins the text, and fails when
an upgrade words it otherwise.

### Adding a voter

The leader refuses an ID that a member has, and an address at which a member
answers already: the member's address as written, or another name of an
ip:port where the member answers. It resolves the host of the new address and
the host of every member, as the transport dials them, an IP to itself, and
refuses a shared ip:port with 409, naming the member. A member's host that does
not resolve, such as the name of a dead member, counts for nothing, and a new
address whose host does not resolve is left to the probe, which then fails.
Without this check, a member listed by its IP whose certificate also names its
DNS host could be added a second time under that host: two voters on one node,
four voters on three nodes, so the quorum rises to three and the node's
failure takes two votes, which stops writes (`TestRefusedChangesChangeNothing`,
`TestAddMemberRules`).

It then probes the address (`Admin.Probe`): it dials it as the transport dials
a peer, within 5 s, and over TLS checks the peer's certificate against the
cluster CA for the host of the address as written, as raft does. It then
closes its writing side and reads until the peer closes, so a peer that refuses
this node's certificate, which TLS 1.3 tells only after the client's side of
the handshake, fails the probe too. A node it probes logs no error for it
(`TestProbeDialsAsRaftDoes`). On plain TCP, with `allow_insecure_transport`,
the probe proves less: only that something accepted the connection and closed
it at the end of the stream; a node that runs TLS passes it, and then cannot
talk to the cluster. A failed probe changes nothing:
`cluster.ErrPeerUnreachable`, a failed precondition, which the API answers 422.
Without it, a typo would add a voter nobody reaches, which raises the quorum:
three voters need two, four need three. hashicorp/raft v1.8.0 makes the new
server a voter at once, with no staging step, and it catches up from the
leader's log or a snapshot.

A change that lost leadership while it committed (`cluster.ErrLeadershipLost`)
can still take effect: the service logs one WARN, `cluster membership change
outcome unknown`, with the actor, the operation, the member and `force`, the
fields of the INFO line of a change that committed
(`TestAChangeOfUnknownOutcomeIsLogged`); read the status to know.

### Removing a member

A removal that leaves fewer than three voters needs `force`, and the last
voter cannot be removed, force or not: hashicorp/raft refuses a configuration
without a voter (`checkConfiguration` in v1.8.0).

The leader asked to remove itself first transfers its leadership, with
hashicorp/raft's `LeadershipTransfer`, which picks the voter with the highest
next index and returns once the node left its leader loop. While it transfers,
the leader refuses every write, barrier and configuration change with
`ErrLeadershipTransferInProgress`, which `clusterError` gives as
`cluster.ErrNotLeader` (see "Follower writes"). The node then waits, up to
5 s, until it knows the new leader, and answers that the leadership moved,
having removed nothing: the API answers 503 with `X-Leader`, and the removal is
repeated at the new leader, where it removes a follower. When the wait ends
with no leader known, the 503 has no `X-Leader` and says to retry. So no
removal commits while the node leads: hashicorp/raft makes a leader that
commits its own removal step down only then, and its followers would wait an
election timeout for the next leader. A transfer can pick a dead voter, when
nothing was written since it died: it then fails and nothing is removed, so
remove dead members first. A transfer that fails on its timeout may have told
its target to start an election already, so the node may lose its leadership
after the failure all the same. `TestRemoveLeaderTransfersFirst` shows the
order on three nodes: after the first call another node leads while every node
still lists the old leader, and after the repeated one the old leader is out
of every member's configuration, its raft never logged `removed ourself`, and
writes commit.

### Replacing a dead member

A dead member is replaced by a removal and then an addition, which is what
`TestReplaceDeadMember` does on three nodes:

1. `DELETE /api/v1/cluster/members/<id>?force=true` at any node: on a
   three-node cluster the removal leaves two voters, so it needs `force`.
2. Start the replacement with `cluster.raft.join: true` and an empty
   `data_dir`, and a certificate of the cluster CA for its advertise host.
3. `POST /api/v1/cluster/members` with its `node_id` and advertise address:
   the leader probes the address, adds the node as a voter, and the node
   catches up from the leader's log or a snapshot.
4. Keep `join` set on the replacement until its `discovery.peers` lists it
   (see "Joining a running cluster").

### A removed node

A removed node's raft keeps running, outside the cluster. hashicorp/raft shuts
a node down at its removal (`ShutdownOnRemove`, which podium leaves on) only
when the node commits its own removal as the leader, which the transfer
avoids. The leader stops replicating to a member as soon as it dispatches the
member's removal, so the node may never learn that it was removed. It cannot
disrupt the members: they refuse its pre-votes, as it is not in their
configuration. Once it no longer hears from a leader its writes answer 503,
while its reads answer from a state that no longer follows the cluster. It
logs election WARNs, and every member logs a WARN for each pre-vote it
refuses, until the node's process is stopped, which the operator must do.

## Transport security

The transport runs mutual TLS 1.3 (`tls_stream.go`): a `hraft.StreamLayer`
that wraps each TCP connection it accepts in `tls.Server` and dials with
`tls.DialWithDialer`, given to hashicorp/raft through
`NewNetworkTransportWithConfig`. A node presents its one certificate,
`Config.TLS`, both ways, and verifies the peer's against the cluster CA both
ways: it answers no RPC from a peer with a certificate of another CA, with
none, over TLS 1.2 or over plain TCP, and gets none through to a peer with a
certificate of another CA, over TLS 1.2 or over plain TCP, so such a node
cannot join (`TestRaftRefusesPeerWithoutClusterCert`). A dialed peer's
certificate must also name the host the node dials, its address in the raft
configuration, compared as written (`TestRaftRefusesPeerCertForAnotherHost`).
Inbound, any certificate the CA verifies is a member, whatever it names: no
check of a connecting peer against the configured peers exists, so the CA must
sign raft node certificates only.

An accepted connection's handshake runs on its first read or write, in the
goroutine hashicorp/raft gives that connection, never in its one accept loop.
It must finish within `handshakeTimeout` (10 s), or the connection is closed:
a host that connects and stays silent, or stops halfway through its
ClientHello, holds that goroutine, the connection and hashicorp/raft's 256 KB
read buffer no longer. The bound ends with the handshake, so a connection may
then sit idle between RPCs (`TestRaftBoundsInboundHandshakes`). The dial
timeout covers the handshake of a connection the node dials.

The stream layer gives raft the node's advertise address as written, not as
it resolves: raft takes it for the node's own address, which a leader
announces, so every node knows its leader by the address the configuration
lists, the host a follower's HTTP forward checks the leader's certificate for
(`TestNodesKnowTheLeaderByItsAdvertiseAddress`). The address is resolved only
to check that peers can dial it.

`New` reads the TLS files before it makes the data directory: a file it cannot
use fails the start with nothing made, in an error that names the file, never
what it holds (`TestNewRefusesUnusableTLSFiles`). It then checks the node's
own certificate as its peers will, against the CA, now, for server and for
client authentication, and for the host of the advertise address as written,
and fails the start with an error that says which check failed
(`TestNewChecksItsOwnCertificate`). Plain TCP, the same stream layer without
TLS, needs `AllowInsecureTransport` and no TLS file, and logs one WARN at
start; without either, `New` refuses the node
(`TestPlainTCPNeedsAllowInsecureTransport`), as `cluster.Config.Validate` does
earlier. See "Raft transport security" in `internal/config/README.md` for the
certificates.

Every accepted connection, over TLS or plain TCP, also answers a configuration
request before hashicorp/raft reads it, and passes every RPC through untouched;
see "Configuration drift" below.

## Configuration drift

Some settings must be the same on every replica, or the nodes act otherwise
whichever of them a request reaches: a token one signed fails on another, a
proxy one trusts is a client on another. They come in four groups, by the names
the logs give them:

| Group        | Settings                                          |
|--------------|---------------------------------------------------|
| `auth`       | `auth.jwt_secret`, `auth.cookie_secure`           |
| `agent auth` | `agent_auth.require`, `agent_auth.enroll_secret`  |
| `proxies`    | `trusted_proxies`                                 |
| `CORS`       | `cors.*`                                          |

`Config.ConfigDigests` holds one digest per group (`cluster.Digests`), which
`app` computes in raft mode only from the node's configuration
(`cluster.SharedSettings`): the SHA-256 of a canonical encoding of the group,
which starts with `podium-config-fingerprint-v1` and the group's name and writes
each setting in a fixed order, every string and list after its length. A secret
enters it only as HMAC-SHA256(secret, `podium-config-fingerprint-v1`), an unset
one included, so no digest holds a secret or its plain SHA-256
(`TestFingerprintRevealsNoSecret`). Settings that apply alike encode alike
(`TestFingerprintChangesWithEachSetting`):

- `auth.cookie_secure` as written (`auto` or `always`, all validation allows);
  `agent_auth.require` and `cors.allow_credentials` as booleans;
- `trusted_proxies` as the set of prefixes the client IP resolver trusts
  (`clientip.ParseTrusted`): trimmed, an IP as its /32 or /128, IPv4-mapped
  entries unmapped, prefixes masked, sorted, without duplicates. Overlapping or
  adjacent prefixes are not merged: `10.0.0.0/8` and `10.0.0.0/8, 10.1.0.0/16`
  trust the same addresses and still count as different;
- `cors.allow_origins` as the set of origins as written, as CORS compares them
  with a request's `Origin` exactly;
- `cors.allow_methods`, `cors.allow_headers` and `cors.expose_headers` with the
  defaults CORS applies to an empty list, as the sets of names their header
  lists carry: the entries joined and split on commas, trimmed, empty names
  dropped, header names in lower case, methods as written;
- `cors.max_age` with CORS's 12 hours for zero, in the whole seconds CORS sends.

Every setting of `cors` counts even while `allow_origins` is empty, when the
others have no effect. `cluster.Fingerprint` is the hex SHA-256 over the four
digests in order. The status, `GET /api/v1/cluster`, shows it as
`config_fingerprint`, behind `cluster:get` (see "Cluster" in
`internal/handler/README.md`); the group digests travel only from node to node,
and no log line holds a digest, the fingerprint or a value. Whoever reads the
fingerprint and knows the other settings can still confirm a guess of a weak
secret offline, by computing the fingerprint of the guess, as the encoding is
public: so the secrets must be strong, random values, not words.

### The configuration request

A node fetches a peer's digests over the raft port, authenticated as raft is,
by mutual TLS: a follower has no credential of the API. The request is one
byte, `0xF0` (240), as the first byte of a connection. hashicorp/raft reads
that byte as the type of an RPC, and v1.8.0 numbers its types from 0 to 4
(`rpcAppendEntries` to `rpcRequestPreVote`, by iota in `net_transport.go`), so
the stream layer answers the request itself (`answerConn`), on the first read
of the connection that gives data, which over TLS comes only after the
handshake verified the peer's certificate. It writes the answer, closes the
connection and returns the end of the stream, which hashicorp/raft's
`handleConn` drops without a log line. Any other first byte, and every later
byte, passes through untouched, so raft's RPCs of every type, and the probe of
a membership change, which reads a quiet end of stream, work as before
(`TestRaftPortAnswersConfigRequests`, `TestOnlyTheFirstByteIsAConfigRequest`).
The answer has a fixed format, version 1: the byte 1, then the four digests in
the order above, 32 bytes each, 129 bytes in all (`TestConfigAnswerFormat`).
The fetch dials the peer as the probe does, sends the byte, and reads until the
peer closes, within 5 s for the dial, the handshake, the request and the
answer; the node's write of its answer has the same bound. A request is the
byte alone: a peer that sends more, which the node leaves unread, may see the
connection reset before it reads the answer.

A peer with a certificate of another CA, with none, over TLS 1.2 or over plain
TCP gets no answer. With `allow_insecure_transport` the answer is as open as
the rest of the raft port: any host that reaches it gets the digests, without a
certificate (`TestPlainTCPAnswersConfigRequests`), which is one more reason to
keep that port to the cluster's nodes. A node without digests, such as one of a
version without the check, answers nothing: its hashicorp/raft refuses the byte
as an unknown RPC type, with one ERROR line, `failed to decode incoming command`
with `unknown rpc type 240`, and closes the connection
(`TestNodeWithoutDigestsAnswersNoConfigRequest`).

### The check

The `config-check` runner, which `app` adds in raft mode, compares the node's
digests with its leader's (`Admin.ConfigCheck`). It checks once a leader other
than this node is known, and again at each change of the leader the node
knows, which hashicorp/raft observes, a flap through no leader included: a
drifted node is caught as the follower of a leader that shares the settings,
and as the leader of followers that do. A node that leads, or knows no leader,
checks nothing. Its lines carry `runner=config-check`, `node_id` and the
leader's `leader_id`:

- a mismatch is one ERROR, `configuration check: this node's configuration
  differs from the leader's in settings that every replica must share`, with
  `groups`, the names of the groups that differ, once per leader and digest set:
  the same leader seen again with the same digests logs it at DEBUG only;
- a match is a DEBUG line, `configuration check: this node's configuration
  matches the leader's`, or an INFO line when it follows a mismatch or a failed
  fetch logged since the last match;
- a fetch that fails is one WARN per check of a leader, `configuration check:
  cannot fetch the leader's configuration digests: retrying, at most every
  minute`, with the error; the check retries after a second, a pause that
  doubles after each failure up to a minute, until a fetch succeeds or the
  leader changes, which ends the pause
  (`TestConfigCheckLogsEachMismatchOnce`, `TestConfigCheckRetriesAFailedFetch`).

`TestConfigCheckFollowsTheLeader` moves the leadership of three nodes over
mutual TLS, two of which share their settings, by hashicorp/raft's transfer to a
chosen node, and shows each of these lines where it belongs, the drifted node
leading included; `TestMismatchedReplicaLogsError`, in `internal/app`, runs
three whole nodes as main runs them. Standalone digests, serves and checks
nothing.

The check covers exactly the settings above. Nothing compares the others: the
settings of the runners that only the leader runs, `lifecycle.*`, `sync.*` and
`history.*`, which change behaviour when the leadership moves, and every other
setting, such as `auth.issuer`, `auth.audience` or the token lifetimes, may
differ between nodes without a line. Run every node with one configuration.

During a rolling upgrade from a version without the check, a follower of the
new version whose leader runs the old one logs the WARN, as the old leader
answers nothing, and retries, at most every minute; the old leader logs one
ERROR, `unknown rpc type 240`, for each attempt. A later version that changes
the encoding would change every digest, so nodes of the two versions would log
the ERROR for every group until all run the same one.

## Data directory

The data directory and its `snapshots` directory are made 0700, and `New`
refuses either, before it opens anything there, the log of a node with state
included, when its group or others have any permission on it or it is a
symlink, written with a trailing slash or not, as the durable store does
(`TestRaftRefusesLooseDataDir`). raft-boltdb makes its files 0600, but
hashicorp/raft makes each snapshot directory 0755 and its `meta.json` and
`state.bin` 0644, so the snapshot store takes their group and other
permissions off as soon as `FileSnapshotStore.Create` returns, before any state
is written into them, and again once `Close` has put the snapshot in place.
hashicorp/raft creates the snapshot a leader installs through the same
`Create`. `New` does the same to the snapshots already there
(`TestRaftFilesArePrivate`, `TestSnapshotsArePrivateWhileWritten`).

## Follower writes

A write that reaches a node that is not the leader changes nothing and fails
with `raft.ErrNotLeader`: a single operation, which is a transaction of one
operation, and a transaction, which fails before its function runs.
`raft.ErrNotLeader` is `cluster.ErrNotLeader`, which lives in the `cluster`
package so that the transports can tell it without importing raft (`errkind`
cannot: raft depends on it through `domain/wire` and the proxy); the errors wrap
it, so `errors.Is` finds it. `errkind.Classify` gives it `Unavailable`, so gRPC
answers `codes.Unavailable` and HTTP answers 503.

The other errors of hashicorp/raft that mean the cluster cannot take a write
now are given a meaning in the same way (`clusterError`), and still are the
hashicorp/raft error for `errors.Is`: `ErrLeadershipTransferInProgress`, which a
leader gives every write while it hands its leadership over (see "Removing a
member"), is `cluster.ErrNotLeader`, as the write changed nothing;
`ErrLeadershipLost` is `cluster.ErrLeadershipLost`, and `ErrEnqueueTimeout` and
`ErrRaftShutdown` are `storage.ErrUnavailable`. They classify as `Unavailable`
too (`TestClusterError`). A write that fails with `cluster.ErrLeadershipLost` is
ambiguous, as it may still commit, so only a write that is safe to repeat should
be retried. The barrier that starts a transaction writes nothing, so one that
fails with `ErrLeadershipLost` is `ErrNotLeader`, which is safe to retry
(`TestAwaitBarrierLeadershipLostIsNotLeader`).

For HTTP the 503 of `ErrNotLeader` and `ErrLeadershipLost`
(`response.FromError`) carries `X-Leader`, as the `Leader` middleware hands
every request it passes through the means to ask where the leader is
(`TestFollowerWriteIsUnavailableWithLeaderHint`). What the header means, and
when it is not sent, is described once, under "Leader hint" in
`internal/transport/README.md`.

The `Leader` middleware forwards the writes of a follower, so a follower's
handlers meet the error only when leadership moves under a request that began
on the leader, or when a read writes. The write handlers of the API answer a
failed store with `response.StoreFailed`, or with `response.FromError`, and log
a leadership error as a WARN: 503 with `X-Leader`, where every other error is
answered and logged as before
(`TestWriteHandlersAnswerLeadershipErrorsWithTheLeader`). The logout and the
HTTP discovery sync still answer `response.Unavailable` for every error, 503
without the header. A gRPC discovery sync on a follower is forwarded to the
leader too, see "Forwarding and trusted hosts" below.

A read that writes is the refresh of a browser session with
`auth.rotate_refresh` on: the `Auth` and `OptionalAuth` middleware refresh a
request whose access token is missing or expired, whatever its method, and the
rotation of the refresh token is a transaction. On a follower it fails with
`ErrNotLeader` before it consumes the token: the request answers 503 with
`X-Leader`, no cookie is set, and the follower logs a WARN from `Auth`, not an
ERROR. On an API route the access log wraps `Auth` and logs every 5xx at ERROR,
except a 503 that carries `X-Leader`, which it logs at WARN: that header is set
only for a routing condition, so the refresh leaves no ERROR line
(`TestAccessLogLevelFollowsTheStatus`,
`TestFollowerRefreshThroughTheAccessLogLogsNoError`). The same cookies then
refresh at the leader (`TestRotatingRefreshOnAFollowerAsksForTheLeader`, and
`TestRefreshOnAFollowerAsksForTheLeader` in the middleware package). So with
`auth.rotate_refresh: true` a raft deployment needs leader-sticky routing: its
load balancer must send browsers to the leader, or retry a request that gets
`X-Leader` on the node it names. With `auth.rotate_refresh` off, as it is by
default, a refresh only reads and works on every node.

## Forwarding and trusted hosts

A follower sends the writes it gets to the leader, at the host of the leader's
raft address, as written, and the port of the listener the write came in on,
which every node is assumed to serve that listener on (`cluster.LeaderAddr`,
see "Cluster" in `internal/config/README.md`). The HTTP listeners forward with
the `Leader` middleware; gRPC discovery forwards a unary `DiscoverService.Sync`
with the `UnaryLeader` interceptor's `Forwarder`, through the generated client:

- over TLS with `tls.client` when `tls.server` is set, checked for the leader's
  raft host and presenting `tls.client`'s certificate when the leader asks for
  one, as the HTTP forward does;
- with the caller's own deadline, and none when the caller set none;
- with `authorization`, unchanged, the only metadata the discovery handler
  reads, the request ID, so that both nodes log the call under one,
  `x-forwarded-for`, the caller's own values with its peer appended, and
  `x-podium-forwarded`, and nothing else of the call's metadata;
- over one client connection, kept while the leader keeps its address, replaced
  when the leader moves, which closes the former leader's, and closed when the
  node shuts down (`TestForwarderKeepsOneConnectionPerLeader`).

The leader's answer, an error included, is the follower's
(`TestFollowerForwardsGRPCSync`, `TestUnaryLeaderForwardsAWriteWithItsMetadata`,
`TestForwardedCallKeepsTheCallersDeadline`). A forward the leader never
answers, as it cannot be reached, is `Unavailable` `leader unreachable` with the
`x-leader` hint, the leader's gRPC address. A follower never forwards a call
that carries `x-podium-forwarded`: it answers it `Unavailable` with the hint, so
a call crosses one node at most, even when a stale view of the leader or nodes
that serve gRPC on different ports would send it round
(`TestUnaryLeaderAnswersWithTheLeadersGRPCAddress`). The follower that forwarded
the call passes that answer on with its hint, the refusing node's view of the
leader. An HTTP forward carries `X-Podium-Forwarded`, which the `Leader`
middleware of a follower answers the same way, 503 with `X-Leader`
(`TestLeaderForwardsAWriteOnce`). Either mark that a client sets itself only
keeps its own request from being forwarded, and the leader serves a marked
request as any other. Either mark counts when it is present, whatever its value.
`UnaryLogger` logs a `not the leader` answer with the hint at WARN, as the
access log does a 503 with `X-Leader`, and `leader unreachable`, which keeps its
hint, at ERROR, as the access log does the 502 of an unreachable leader
(`TestUnaryLoggerLogsRoutingAnswersAtWarn`).

The HTTP forward also drops the CORS headers of the leader's answer, which the
reverse proxy would add to the follower's own: the answer names the client's
origin once (`TestForwardedWriteCarriesTheCORSHeadersOnce`).

The leader then counts the agent where the agent is: the client IP resolver
trusts the hosts of the node's current raft configuration as proxies, besides
`trusted_proxies`. Their `X-Forwarded-For`, which the `Leader` middleware's
reverse proxy appends the caller to, and their gRPC `x-forwarded-for` count, so
the discovery limiter keys the agent's token failures on the agent's own
address, not on the follower's (`TestForwardedSyncKeepsClientIPForLimiter`),
and so does the login limiter on a browser's. A host that is in neither list is
its own client, whatever it sends (`TestUntrustedForwardedHeaderIgnored`). Trust
is decided per request from the TCP peer, as for `trusted_proxies`:

- The configuration is read anew on every check (`Leadership.Members`, a read
  of the copy hashicorp/raft keeps apart from its main loop,
  `TestLeadershipListsTheMembers`), so a host that leaves it is trusted no
  more, and one written as an IP that joins it is trusted, at once
  (`TestHostsFollowTheListAndTheLookups`).
- A host written as a name is trusted at the addresses the name resolves to,
  through the system resolver, as the nodes dial one another. The names are
  looked up in the background, every 10 s and at once when a check meets a name
  not looked up yet, never on the request path; a name whose last lookup failed
  is trusted at no address (`TestHostsFollowTheListAndTheLookups`,
  `TestHostsLookUpANewNameAtOnce`,
  `TestRaftHostNamesAreTrustedAtTheirAddresses`). So the trust of a name is
  only as good as the DNS answers the node gets, and up to 10 s late: a node
  that moves is trusted at its new address after the next lookup, and at its old
  one until then. Raft's mutual TLS does not cover it, as it checks a peer's
  certificate, not which address a name gives. Where DNS cannot be trusted that
  far, write the advertise addresses as IPs.
- The trust is a node's, not a connection's: every process on a node's host can
  name any client in those headers, as behind any trusted proxy, and the address
  the node connects to the others from must be the one its raft address names,
  or resolves to, for its forwards to count. So a node's host must not relay
  outside clients at L4, by a port forward, SNAT or kube-proxy for a
  hostNetwork pod, just as a host in `trusted_proxies` must not pass a client's
  header through: such a client reaches the leader from the host's address, and
  its own forwarding headers pick its client IP.

In standalone the resolver trusts `trusted_proxies` only, and gRPC discovery
reads no forwarding metadata: its client is the peer, as before
(`TestStandaloneGRPCDiscoveryIgnoresForwardedFor`).

The main listener refuses a cross-origin browser mutation before the `Leader`
middleware runs, so a follower answers it 403 itself and the request never
reaches the leader (`TestCrossOriginRefusedOnFollower`).

## Apply failures

A committed command that cannot be decoded, or whose operations fail to apply,
changes nothing on the replica and is dropped, whole, on every replica that
fails on it. `FSM.Apply` returns the error, but only the leader that proposed
the command ever reads it, as the response of `Apply`. So every replica logs the
command at ERROR, with the index of its log entry, its kind and the number of
its operations, and nothing it carries, not the error either, as either can
hold the values of the state:

- `raft: committed command failed to apply and was dropped on this replica,
  which stays as it was (corrupted entry or version skew)`, with `index`, `kind`
  (the names in the protocol of its operations, each once, in order:
  `agent_upsert,agent_delete`) and `ops`;
- `raft: committed command is not decodable and was dropped on this replica
  (corrupted entry or version skew)`, with `index` and the kind `undecodable`,
  and no count.

`Store` checks every write against the state before it proposes it. A
transaction runs on an overlay of the leader's store, and a single-operation
write is a transaction of one operation, as in the durable store: the overlay
refuses what the FSM would refuse when it applies the operation. So the delete
of an absent row gives its caller `ErrNotFound` and no command reaches the log,
and a bulk delete that matches nothing proposes nothing
(`TestSingleWritesAreValidatedBeforeTheyAreProposed`). Like every transaction it
starts with a barrier, an entry that carries no write: every accepted single
write now costs a barrier entry plus its command, two log entries and two round
trips, a refused one a barrier alone, and `snapshot_threshold` counts entries,
barriers included. The session clock the operations carry is the overlay's
(`TestSingleWritesKeepTheSessionClock`). So a command that fails to apply is a
corrupted entry or a version skew: a node that does not know an operation
cannot apply a command a newer node proposed, and the replicas diverge, which
the line is the only sign of (`TestFollowerLogsFailedApply`,
`TestFSMApplyLogsOnlyTheShapeOfAFailedCommand`). A restart replays the log
entries since the last snapshot, so a command that failed is logged again at
each restart until a snapshot compacts it away; the `index` tells it is the
same entry.

## Log compaction

`cluster.raft.snapshot_interval`, `snapshot_threshold` and `trailing_logs` are
`Config.SnapshotInterval`, `SnapshotThreshold` and `TrailingLogs`, and
hashicorp/raft's own settings of the same name: every `SnapshotInterval` (120 s
by default, in fact a random time of between one and two intervals) the node
checks whether `SnapshotThreshold` log entries (8192) have piled up since its
last snapshot, and takes one if so; the snapshot then truncates the log down to
the last `TrailingLogs` entries (10240), which a follower that is slightly
behind catches up from, in place of a snapshot of the whole state. A zero value
keeps the default, so the settings are hashicorp/raft's until configured
(`TestRaftCompactionDefaultsAreHashicorps`). The configuration refuses a
negative value and an interval under 5 ms, the minimum of hashicorp/raft's
`ValidateConfig`, which has no other limit on the three settings
(`TestRaftCompactionLimitsAreHashicorps`), so a refused value fails before
anything in the data directory is opened. `TestCompactionKnobsApplied` shows the
values reach
hashicorp/raft's configuration, and that a node with small ones snapshots on
its own and truncates its log to the trailing logs.

## Logging

hashicorp/raft, its snapshot store and its network transport log through the
node's zerolog logger (`logger.go`), in place of the hclog loggers they would
make on stderr: a node writes nothing to stderr
(`TestRaftLogsGoThroughTheNodeLogger`). The adapter keeps hashicorp's INFO,
WARN and ERROR levels and drops its TRACE and DEBUG lines, the elections and
the replication chatter. Each line carries
`profile=raft`, the name of the component that logged it as `component` (`raft`,
`snapshot` or `raft-net`), and the key and value pairs of hashicorp's line as
fields. A key that zerolog or the adapter writes itself, `time`, `level`,
`message` or `component`, gets the prefix `raft_`: raft logs how long a peer has
been out of contact as `time`, which must not be a second timestamp on the line
(`TestHCLogAdapterKeepsTheKeysOfZerolog`). No command payload reaches the log: a
byte slice is only counted, and the value of `command`, under which raft prints
a request it did not expect whole, is `[redacted]`
(`TestHCLogAdapterRedactsPayloads`). The one exception is the kind of a change
of the configuration, such as `AddVoter`, which operators need to see next to
the ID and the address of the server it changes
(`TestHCLogAdapterShowsConfigurationChanges`).

Every connection that the transport refuses or that times out in its handshake
is one ERROR line, `failed to decode incoming command` from `raft-net`, with the
TLS error. That is expected of a host that is no cluster node and reaches the
raft port, and nothing limits it: a connect flood is a flood of lines. So the
raft port, `cluster.raft.bind_addr`, must be reachable only from the cluster's
nodes, by firewall or network policy. TLS keeps other hosts out of the cluster,
not off the port. A configuration request that the node answers leaves no line
(see "Configuration drift").

## Multi-node tests

`loopback_test.go` holds the helper that multi-node tests share.
`newLoopbackCluster(t, ids...)` reserves a loopback port for each node, then
starts every node through `New` on mutual TLS, each listing all of them as
`id=127.0.0.1:port` peers, with election and heartbeat timeouts of
`loopbackTimeout` (250 ms). Each node presents a certificate for 127.0.0.1 of a
CA the helper makes when the test runs; no key is kept in the repository.
`loopbackNodes` does the same without starting them, for a test that changes a
node first. `leader` waits for exactly one ready leader, and `write` runs a
write on it, again on the next leader if leadership moves first. A node's
`stop` and `start` restart it from its data directory (`dir`), with any
discovery; options to `start` edit the node's `Config`, such as `withTLS`,
`insecureTransport` and `withConfigDigests`, and `open` returns the error of a start a test expects to
fail. `capture` makes a node log into a `logBuffer`, which is safe to read while
the node runs, before it starts: raft's goroutines log through the node's
logger. The helper's waits poll a condition until a timeout, never a fixed
sleep.
