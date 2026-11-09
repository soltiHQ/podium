# internal/handler
HTTP and gRPC request handlers for the control plane.

## Package map
```text
handler/
├── handler.go      package documentation
├── api.go          API — REST + HTMX endpoints (users, agents, specs, sessions, roles)
├── api_cluster.go  API — the cluster status and membership routes of a raft node
├── discovery.go    HTTPDiscovery + GRPCDiscovery — agent heartbeat / sync
├── ui.go           UI — full-page HTML renders (login, dashboard, detail pages)
└── static.go       Static — embedded file serving (CSS, JS, images)
```

## Handlers

| Handler           | Transport | Constructor           | Dependencies                                                         |
|-------------------|-----------|-----------------------|----------------------------------------------------------------------|
| `API`             | HTTP      | `NewAPI`              | user, access, session, credential, agent, spec services + proxy.Pool; the cluster service in raft mode (`WithCluster`) |
| `HTTPDiscovery`   | HTTP      | `NewHTTPDiscovery`    | agent service, discovery limiter, sync buckets, client IP resolver   |
| `GRPCDiscovery`   | gRPC      | `NewGRPCDiscovery`    | agent service, discovery limiter, sync buckets, client IP resolver (raft mode) |
| `UI`              | HTTP      | `NewUI`               | access + spec services, client IP resolver, cookie Secure policy     |
| `Static`          | HTTP      | `NewStatic`           | embedded `ui.Static` filesystem                                      |

All constructors panic on nil dependencies — fail-fast at startup. The client IP resolver is the exception:
nil means no trusted proxies.

## Route registration
```text
mux (http.ServeMux)
│
├── API.Routes(mux, auth, _, common...)
│     auth enforced at mux level, permissions per-method inside handler
│
├── UI.Routes(mux, auth, optionalAuth, perm, common...)
│     perm(kind.Permission) added per-route at mux level; optionalAuth guards /logout
│
├── HTTPDiscovery.Sync  ← registered externally (no Routes method)
│
└── Static.Routes(mux)  ← no middleware
```

## Middleware model
```text
  API:   common → auth → handler → middleware.RequirePermission(kind.*)
                                    └── per-method, wraps individual http.HandlerFunc

  UI:    common → auth → [perm(kind.*)] → handler
                          └── per-route, added at mux registration
```
API checks permissions **inside** handler methods (per-action granularity).
UI checks permissions **at registration** (per-route granularity).

## API endpoints
`TestRoutePermissionMatrix` (`route_permissions_test.go`) holds this table as code and checks it against the
registered routes: every new `/api/v1` route needs a row there.

Endpoints that read a JSON body (user create, update and password, agent labels, spec create and update, cluster member
add) require `Content-Type: application/json`, parameters allowed; any other type gets 415 `Content-Type must be
application/json.`. A cross-site HTML form cannot send that type. In front of every route, `app` refuses
cross-origin browser mutations with 403 (see `internal/config`, "Browser sessions").

### Users `/api/v1/users`
| Method | Path                          | Permission                                               |
|--------|-------------------------------|----------------------------------------------------------|
| GET    | `/api/v1/users`               | `UsersGet`                                               |
| POST   | `/api/v1/users`               | `UsersAdd`; also `UsersEdit` to set roles or permissions |
| GET    | `/api/v1/users/{id}`          | `UsersGet`                                               |
| PUT    | `/api/v1/users/{id}`          | `UsersEdit`                                              |
| DELETE | `/api/v1/users/{id}`          | `UsersDelete`                                            |
| GET    | `/api/v1/users/{id}/sessions` | `UsersGet`                                               |
| POST   | `/api/v1/users/{id}/disable`  | `UsersEdit`                                              |
| POST   | `/api/v1/users/{id}/enable`   | `UsersEdit`                                              |
| POST   | `/api/v1/users/{id}/password` | `UsersEdit`; 12 to 72 characters, else 400               |

`PUT /api/v1/users/{id}` is a patch applied inside the transaction. A non-empty `subject`, `name` or `email` and a
present `role_ids` or `permissions` list (`[]` clears it) replace the stored value; everything else stays as the
transaction reads it. Fields the request does not set keep a change committed meanwhile, and `disabled` always does
(see disable and enable). A present `role_ids` or `permissions` list overwrites the stored set, so of two edits of one
user the later one's lists win, unless it names with `If-Match` the version it is based on (below).

`GET /api/v1/users/{id}` answers `ETag: "<tag>"`, and the UI edit form carries the same tag. The tag is the lowercase
hex of the first 16 bytes of SHA-256 over every stored field of the user: ID, subject, email, name, disabled, role IDs,
permissions, creation time and update time, with strings and lists length prefixed. It is computed, never stored. It
does not rest on the update time alone, which two changes may share when they read the same clock and which a writer
may set, so it changes whenever any stored field does, whichever write made the change: update, disable or enable. A
write that stores what is already stored leaves it as it was. A change to the permissions of a role is not a change of
the user and leaves it too.

`PUT` takes the tag as `If-Match`, read the way Deploy reads it (see Specs): RFC 9110 lists over every `If-Match`
header, strong tags with or without their quotes; a weak tag never matches, and a blank header names no tag. The
update transaction compares the list with the user it reads, after the target ceiling (a caller who may not manage the
user gets 403 whatever the tag) and before the patch is applied. When none of the tags names the stored user, nothing is
written and the answer is 412 `User changed since it was read; reload and retry.`; a user deleted meanwhile is still
404. Without `If-Match`, or with `*` anywhere in it, the patch is applied to the stored user as it is, as before: API
clients that never read before they write keep working, and of two such edits the later one's role and permission
lists win. A client that sends those lists should send the tag. Only the update takes `If-Match`; disable, enable,
delete and the password route do not. The UI edit form always sends the tag of the user it shows, so a form that went
stale is refused instead of reverting a concurrent role or permission change (see `ui/README.md`).

### Sessions `/api/v1/session`
| Method | Path                          | Permission  |
|--------|-------------------------------|-------------|
| POST   | `/api/v1/session/{id}/revoke` | `UsersEdit` |

### User-management ceilings
The user, credential and session services enforce these inside the transaction of the mutation:

- **Grant ceiling.** Create and update may give a user, directly or through roles, only permissions the caller holds → 403.
  A permission name outside `enum.All` in the request → 400; one already stored grants nothing and never blocks the caller.
- **Target ceiling.** Update, disable, enable, delete, password and session revoke on another user need every permission of that user → 403.
  On yourself the self rules apply instead: no change of your own roles or permissions, no self delete or disable → 400.
- **Last admin.** A delete, disable or role/permission change that would leave no enabled user holding every permission → 409.

### Agents `/api/v1/agents`
| Method | Path                                             | Permission     |
|--------|--------------------------------------------------|----------------|
| GET    | `/api/v1/agents`                                 | `AgentsGet`    |
| GET    | `/api/v1/agents/{id}`                            | `AgentsGet`    |
| DELETE | `/api/v1/agents/{id}`                            | `AgentsDelete` |
| PUT    | `/api/v1/agents/{id}/labels`                     | `AgentsEdit`   |
| POST   | `/api/v1/agents/{id}/credential`                 | `AgentsReset`  |
| GET    | `/api/v1/agents/{id}/tasks`                      | `AgentsGet`    |
| GET    | `/api/v1/agents/{id}/tasks/{taskID}`             | `AgentsGet`    |
| POST   | `/api/v1/agents/{id}/tasks/{taskID}/cancel`      | `AgentsEdit`   |
| GET    | `/api/v1/agents/{id}/tasks/{taskID}/logs/stream` | `AgentsGet`    |
| GET    | `/api/v1/history`                                | `AgentsGet`    |

`POST /api/v1/agents/{id}/credential` (no body) issues the agent a new credential in place of the one it holds, in one
transaction. From its commit discovery refuses the old token (401 / `Unauthenticated`), and podium's calls to the agent
present the new one, so the old token ends in both directions at once (`TestResetCredentialRevokesBothDirections`). The
new credential keeps the enrollment time.

- With an enrollment secret set it is a derived credential (see `internal/config`, "Enrollment secret"), and it names
  the current secret, whatever secret the old one derived from. Its generation is the later of the old generation plus
  one (a stored credential has none and counts as 0) and the time of the reset in Unix seconds, at least 1 and below the
  last generation (`TestResetCredentialIssuesTheLaterOfTheNextGenerationAndTheTime` and `TestResetTakesTheTimeFloor` in
  `internal/service/agent`). The time is there because the old generation can be below one that was issued and revoked:
  a stored credential counts as 0 though its agent held derived ones before (a secret removed, a reset while none was
  set, the same secret set again), and a restored backup brings a credential back below the generations reset since
  (`TestResetAfterAStoredInterludeRevivesNoGeneration` and `TestResetAfterARestoredBackupExceedsEveryGenerationIssued`).
  The generation is above every earlier one as long as each reset of the agent falls in a later second than the one
  before it, on the clock of the leader, the node that serves the reset; a reset in the same second as the last takes
  the generation after it. The answer is `{"agent_id", "token", "generation"}`, and `podium agent-token --generation
  <generation> <agent-id>`, run with the secret, prints the token again.
- Without a secret it is a stored credential, as trust on first use stores tokens, with a new random token of 32 bytes
  as 64 lowercase hex digits (`TestResetWithoutSecretIssuesRandomToken`). The answer has no `generation`, and nothing
  derives that token again.
- 404 when the ID has no credential, with or without an agent row. 409 while `agent_auth.require` is false, where
  podium checks and presents no token, so there is nothing to reset and nothing changes (`TestResetRefusals`). 409 too
  for a derived credential at the last generation (4294967295): the next would wrap around to 0, the token its agent ID
  enrolls with.

No response, event or log other than this answer holds the token. The answer carries `Cache-Control: no-store`; the
event `agent_credential_reset` holds the agent ID, its name and the actor, no token; no log line holds one, and no read
returns it. A reset without a secret also keeps its random token in the state, as every stored token is kept, replicated
in raft mode and in backups; a derived credential holds no token. A token that was not kept is replaced by another
reset.

Once the reset is committed, the node that served it, the leader in a raft cluster since followers forward the write,
closes its pooled gRPC connection to the agent (`Pool.Forget`); a call in flight on it fails, a log stream included.
That is all it closes: an HTTP agent's open log stream (SSE), and the pooled connections of the followers with the log
streams open on them, keep running. No pool keeps a token, so every new call to the agent, on any node that holds the
reset, carries the new token. The agent has to be given the new token: until it is, podium refuses its syncs, and the
calls podium makes to it carry a token that it does not hold.

`DELETE /api/v1/agents/{id}` removes the agent's row, if it has one, and every rollout of the agent ID in one
transaction, so that it never loses one and keeps the other (`TestDeleteIsAtomic` in `internal/service/agent`). An agent
that lifecycle deleted has no row and keeps its rollouts (see `internal/server/README.md`): its delete drops them
(`TestDeleteDropsTheRolloutsOfAnAgentWithoutARow`). Its credential and its execution history stay. It answers 204 with
`HX-Redirect: /agents`, and 404 only when the ID has neither a row nor a rollout. Once the delete is committed it closes
the serving node's pooled gRPC connection to the agent the same way, records `agent_removed` with the actor and the
agent ID, and closes the agent's open `agent_inactive` and `agent_disconnected` issues, as a reconnect does, recording
`issue_closed` by the actor when it closed one. `agent_removed` is no issue kind, so the dashboard lists an operator's
delete in its activity only; lifecycle's deletion records `agent_deleted`, which stays an issue. An agent that still
runs registers again at its next sync with its credential, as a new agent with no labels and no rollouts
(`TestDeletedAgentReturnsWithoutRollouts`). To keep it out, reset its credential first: its old token is then refused,
and only the new one brings it back.

Both are leader writes. A follower forwards the reset like any write (`TestResetThroughFollowerForwards`, in
`internal/app`); no test sends the delete through a follower, which forwards it by construction: the `Leader` middleware
forwards every `DELETE`, as every `POST`, `PUT` and `PATCH`. A write that reached a follower or lost leadership answers
503 with `X-Leader` (`TestWriteHandlersAnswerLeadershipErrorsWithTheLeader`, both routes).

`AgentsDelete` and `AgentsReset` are in `enum.All`, so the Admin role holds them, and no other built-in role does
(`TestOnlyAdminMayResetOrDeleteAnAgent`). As with `ClusterGet` and `ClusterManage`, a user who holds every permission
directly, not through the Admin role, does not gain them: bootstrap gives them to the Admin role only, and such a user
needs both to keep counting as an administrator. `AgentsEdit` gives neither action.

The task log stream and the event stream (see "Other") run their route's access check again every keepalive interval
(15s), whether they wrote or not: the session is live, the user enabled and, for task logs, still holds `AgentsGet`.
`app` builds both checks from the session service with `middleware.Recheck`. A failed check, or one that cannot be made
(a storage error), ends the stream: within one check period of a revocation, plus at most one blocked write (up to
30s). `EventSource` then reconnects through authentication and gets 401 or 403, or 503 if the store is unavailable.
Before every write a stream sets its write deadline 30s ahead, so `http.write_timeout` ends no healthy stream. The
deadline drops a client whose write blocks, that is, whose buffers are full. On a quiet stream the small keepalives can
fit into the buffers for a long time, so a reader that stopped reading is not guaranteed to be dropped at 30s.

### Specs `/api/v1/specs`
| Method | Path                           | Permission               |
|--------|--------------------------------|--------------------------|
| GET    | `/api/v1/specs`                | `SpecsGet`               |
| POST   | `/api/v1/specs`                | `SpecsAdd`               |
| GET    | `/api/v1/specs/{id}`           | `SpecsGet`               |
| PUT    | `/api/v1/specs/{id}`           | `SpecsEdit`              |
| DELETE | `/api/v1/specs/{id}`           | `SpecsDelete`            |
| DELETE | `/api/v1/specs/{id}/force`     | `SpecsDelete`            |
| POST   | `/api/v1/specs/{id}/deploy`    | `SpecsDeploy`            |
| GET    | `/api/v1/specs/{id}/sync`      | `SpecsGet`               |
| GET    | `/api/v1/specs/{id}/placement` | `SpecsGet` + `AgentsGet` |

The placement answers its plan token twice: as `plan_token` and as `ETag: "<token>"`. The token is the lowercase hex
of the first 16 bytes of SHA-256 over the spec ID, version and generation and, sorted by agent ID, each target's ID,
whether it exists in discovery and is eligible, the runner Deploy would use and the action on its rollout: `install`
(no rollout yet) or `update` (the rollout exists; Deploy brings it to the current generation). Agents that keep only
a rollout to uninstall are not part of it: an agent that leaves the targets already changes it. It is computed, never
stored, and leaves out `checked_at`, heartbeat observations and the sync state of rollouts (status, task, observed
generation), so neither a heartbeat nor a sync changes it. Nor does a completed uninstall, for an agent that is no
longer targeted. An agent targeted again before its uninstall completes (its labels restored, say) is an `update`
step until the uninstall drops its rollout, then an `install` step: that completion changes the token, one 412. A spec
save changes it, through the version, even when generation stays, and so does another deploy that created rollouts.

Deploy takes the token as `If-Match`, read as RFC 9110 lists over every `If-Match` header: strong entity tags, each
with or without its quotes. When the plan recomputed in the deploy transaction has a token that no listed strong tag
names, Deploy writes nothing and answers 412 `Placement changed since it was previewed. Review it again.`. A weak tag
never matches, and a blank `If-Match`, or one of empty list elements only, names no plan and gets the same answer.
Only without `If-Match`, or with `*` anywhere in it, Deploy deploys the current placement unchecked, as before, so API
clients that never preview keep working.

### Dashboard `/api/v1/dashboard`
| Method | Path                       | Permission                                                            |
|--------|----------------------------|-----------------------------------------------------------------------|
| GET    | `/api/v1/dashboard`        | (any authed); each issue and activity entry needs its read permission |
| DELETE | `/api/v1/dashboard/issues` | the edit permission of the issue's subject                            |

Every event kind maps to a subject (`event.SubjectOf`):

| Subject | Event kinds                                                                                                           | Read        | Dismiss      |
|---------|-----------------------------------------------------------------------------------------------------------------------|-------------|--------------|
| agent   | `agent_connected`, `agent_inactive`, `agent_disconnected`, `agent_deleted`, `agent_removed`, `agent_credential_reset` | `AgentsGet` | `AgentsEdit` |
| spec    | `spec_created`, `spec_updated`, `spec_deployed`, `sync_failed`                                                        | `SpecsGet`  | `SpecsEdit`  |
| user    | `user_*`, `session_created`, `rate_limited`                                                                           | `UsersGet`  | `UsersEdit`  |
| unknown | `issue_closed` (it can name any subject) and unknown kinds                                                            | all three   | all three    |

### Cluster `/api/v1/cluster`
| Method | Path                           | Permission      |
|--------|--------------------------------|-----------------|
| GET    | `/api/v1/cluster`              | `ClusterGet`    |
| POST   | `/api/v1/cluster/members`      | `ClusterManage` |
| DELETE | `/api/v1/cluster/members/{id}` | `ClusterManage` |

Only a raft node registers these routes: `app` gives the API its cluster service (`internal/service/cluster`, which holds
the rules) with `WithCluster`. Standalone registers none, so its cluster paths answer as any path that the UI's catch-all
page route takes: 404 to an authenticated GET, 405 to an authenticated POST or DELETE, and 401 without credentials, as
before raft mode had them (`TestClusterRoutesAbsentInStandalone`). `cluster:get` and `cluster:manage` exist in both modes,
and appear in both among the permissions a user may be given, but they guard only these raft-mode routes. Of the built-in
roles only Admin holds them, as it holds `enum.All` (`TestClusterRoutesNeedPermission`). Bootstrap resets a stored Admin
role that differs to its definition (`TestBuiltinRolesReconciled` in `internal/bootstrap`), which it runs at a standalone
start and at each raft leadership term (see `internal/bootstrap/README.md`).

`GET /api/v1/cluster` is a read, so the node that gets it answers it itself, a follower included: its cluster as it sees
it. `node_id`, `role` (`leader`, `follower`, `candidate` or `shutdown`), `leader_id` and `leader_address`, the raft
address of the leader it knows, both left out while it knows none, `members`, each with its `id`, its raft `address` as
the configuration writes it, and its `suffrage` (`voter`, `nonvoter` or `staging`), `last_index`, the index of the last
entry of its log, and `config_fingerprint`, the fingerprint of the settings that every replica must share, as this node
holds them: the hex SHA-256 over the digests of their five groups, `cluster.Fingerprint` (`TestClusterStatusAnswer`).
Nodes whose fingerprints differ do not share those settings, so comparing the answers of every node shows drift that the
nodes' `config-check` runners also log (see "Configuration drift" in `internal/raft/README.md`). The fingerprint is no
secret, but whoever reads it and knows the other settings can confirm a guess of a weak `auth.jwt_secret` or
`agent_auth.enroll_secret` offline, by computing the fingerprint of the guess: it is served behind `cluster:get` only,
and the secrets must be strong.

`POST /api/v1/cluster/members` takes `{"id": "n4", "address": "10.0.0.4:7000"}`, a node's `node_id` and raft advertise
address, and adds it as a voter. `DELETE /api/v1/cluster/members/{id}`, with `?force=true` where the guard asks for it,
removes a member. Both are writes, which a follower forwards to the leader, and both answer 204 once the change committed.
The leader checks a change against its latest configuration, read with its index, and applies it against that index, so
a change of the configuration in between refuses it instead of slipping past the checks. Every refusal changes nothing
(`TestClusterRouteAnswers`):

| Status | When                                                                                                         |
|--------|--------------------------------------------------------------------------------------------------------------|
| 400    | a body without `id` or `address`; an `id` with `/`, `=`, a comma or spaces around it, which the member's route or `discovery.peers` could not name; an `address` that is not `host:port` with a port from 1 to 65535 or names no host; a `force` that is no boolean |
| 404    | no member has the `id` to remove                                                                             |
| 409    | a member has the `id` already; a member answers at the `address` already, as written or under another name of the ip:port it answers at, which the message names with the member, `Member n2 of the cluster answers at 10.0.0.2:7000 already.`: the leader resolves the host of the `address` and of every member, skips a member whose host does not resolve, and refuses a shared ip:port, as one node must not be two voters; a removal that leaves fewer than 3 voters, without `force=true`, which the message names; the last voter, `The last voter cannot be removed.`, force or not; a configuration that changed after the leader read it, `configuration changed, retry` |
| 415    | a body that is not declared as JSON                                                                          |
| 422    | no raft node of the cluster answers at the `address`, which the message names: before it adds a voter, the leader dials the address as the raft transport would and checks the certificate there as raft does, so that a typo never adds a voter nobody reaches, which raises the quorum |
| 500    | the leader cannot read its configuration: a read of its stores that fails, or gives index 0 or no server, logged at ERROR as `cannot read the raft configuration` (see "Membership" in `internal/raft/README.md`) |
| 503    | the write met a follower or lost leadership: with `X-Leader`, as every write (see "Leader hint" in `internal/transport/README.md`); a 404 or 409 of the rules above that the node could not confirm its leadership for, also with `X-Leader`, below; the leader's own removal, below |

The 404 and the 409s of the rules, an unknown member, an `id` or `address` in use, the force guard and the last
voter, come from the leader's own configuration, which a deposed leader reads stale. So before it answers one, the
cluster service asks a quorum whether the node still leads the term it read the configuration in
(`cluster.Admin.VerifyLeader`), and answers 503 with `X-Leader`, as for a follower, when it does not, having changed
nothing (`TestRefusalsFromTheConfigurationConfirmTheLeadership` in `internal/service/cluster`). A change the rules let
through needs no check: a deposed leader cannot commit it.

The leader asked to remove itself first hands its leadership over to another voter, with hashicorp/raft's leadership
transfer, so that writes resume without an election timeout, and removes nothing yet. Once it knows the new leader, it
answers 503 with `X-Leader` and the message `This node handed its leadership over and is still a member: repeat the
request, at the leader that X-Leader names, to remove it.`, the leader the node read once, so the header and the message
agree. The request repeated there, or at the same node, which now forwards it, removes a follower. So the removal of the
leader takes two calls, both of which a follower forwards (`TestMembershipChangesThroughAFollower` in `internal/app`).
When no new leader is known after 5 s, or none can be named, the 503 has no `X-Leader` and says `Leadership moved; no
leader is known yet, retry.`. A transfer that
fails, as one to a dead voter that hashicorp/raft picked, removes nothing and answers 503 without `X-Leader`: remove dead
members first. See "Membership" in `internal/raft/README.md`, which also says how to replace a dead member, and what
becomes of a removed node, whose process must be stopped.

Every change that commits logs one INFO line, `cluster membership changed`, with `actor_user_id`, `operation` (`add` or
`remove`), `member_id`, `member_address` and `force`; a hand-over logs one too, with the `transfer` operation. A change
that lost leadership while it committed, or whose node shut down before its outcome was known, which may still take
effect, logs one WARN line with the same fields instead, `cluster membership change outcome unknown`. No line names a
token.

### Other
| Method | Path                    | Permission                                                           |
|--------|-------------------------|----------------------------------------------------------------------|
| GET    | `/api/v1/permissions`   | `UsersEdit`; lists only the permissions the caller holds             |
| GET    | `/api/v1/roles`         | `UsersEdit`; lists only the roles whose permissions the caller holds |
| GET    | `/api/v1/events/stream` | (any authed); registered by `app`, sends refresh triggers only       |

## Discovery endpoint
| Transport | Method             | Path / RPC                     |
|-----------|--------------------|--------------------------------|
| HTTP      | POST               | `/api/v1/discovery/sync`       |
| gRPC      | `DiscoverService/Sync` | proto-defined                |

Both parse the agent heartbeat payload and call `model.NewAgentFrom`, which checks its field limits before anything
is enrolled or charged. They then look at the bucket of the agent ID (`syncOverRate`, see "Sync rate" below), verify the
token (`verifyAgentToken`) when agent auth is required, charge the sync to the bucket (`chargeSync`), and call
`agentSVC.Upsert`. The capability fields have
limits of their own: at most 64 runners, each name at most 128 bytes; per runner at most 256 labels (key at most 317
bytes, value at most 1024 bytes) and at most 256 workloads (`api_version` at most 317 bytes, `kind` at most 128 bytes).
Data that breaks one is answered 400 / `InvalidArgument` naming the field (such as
`agent capabilities.runners[1].name exceeds 128 bytes`), with nothing enrolled or stored
(`TestCapabilityCapsRefuseOversizedFields`). The SDK treats a 400 as permanent. No byte limit that refuses is below the
SDK's own: there a label key is a qualified name, an optional DNS prefix of at most 253 bytes, a slash and a name of at
most 63, and an `api_version` is a CRD group of at most 253 bytes, a slash and a version of at most 63, so both are 317
bytes at most. The three counts are below what the SDK lets an agent send, since it sets none: an agent with more than
64 runners, or more than 256 labels or 256 workloads in one, gets that 400. A sync with the longest key and
`api_version` the SDK allows and the counts at the caps is stored whole (`TestCapabilityCapsAcceptWhatTheSDKSends`).
`os`, `arch` and `platform` are not refused for their length: each is cut to 256 bytes on a rune boundary
(`TestLongPlatformFieldsAreCut`). The limits are not applied to rows already stored. The upsert reports what
the sync changed in the stored row (`agent.UpsertResult`), and discovery sends the `agent_update` refresh pulse only
when the sync gave the UI pages something new: a **visible change**, that is, a new agent or a different status, name,
endpoint, labels or capabilities, or a **reconnect**, that is, a new agent, an agent that was not active, or one that
reports less uptime than its stored row (`model.Agent.ReconnectedSince`, the rule that also re-arms failed rollouts).
A heartbeat that moves only the time of the sync and the uptime sends none. The hub coalesces the pulses it does send
(`events.coalesce`).

With agent auth required, the first sync of an unknown agent ID enrolls the token it presents (trust on first use) and
sets the endpoint podium calls; every later sync of that ID must present the same token. With an enrollment secret
(`SOLTI_AGENT_AUTH_ENROLL_SECRET`, held by the agent service as `agent.WithEnrollSecret`), the first sync enrolls only
the token derived for that ID at generation 0 (`agent.DeriveToken`, printed by `podium agent-token`), as a derived
credential, which stores no token: any other token is an `ErrEnrollRefused`, an `ErrUnauthenticated` like a wrong one,
so 401 / `Unauthenticated`, nothing stored, one failure for the limiter, and a WARN with the agent ID and the client IP,
never the token. A known ID is checked against its credential: a stored token, of an agent that enrolled without the
secret or that an older version enrolled, as it always was, secret or not (one a former secret derived keeps working);
a derived credential against the token the secret derives for its generation.
With the secret set, a stored token that it derives at generation 0 becomes a derived credential in the transaction of
the check, and a derived credential of another secret that presents the token of the current secret for its generation,
as an agent re-provisioned under it does, moves to the current secret. Any other sync of a derived credential with no
secret set, or of another secret than the current one, is refused as an `agent.DerivedCredentialError`, an
`ErrUnauthenticated` too, with one failure for the limiter and a WARN with the agent ID, the client IP and the error's
text. The text says the credential derives from another enrollment secret, or from one while none is set, and names the
ways back, never a token: the secret it derives from set again; with a secret set, the token that `podium agent-token
--generation` prints under it; and a reset of the credential (`POST /api/v1/agents/{id}/credential`), which issues a
stored token while none is set and a derived one under the current secret otherwise
(`TestChangedSecretRefusesDerivedAgentClearly`); the agent service's token lookup for the agent's calls fails with the
same error, so podium makes no call to it. The agent routes that would call it answer as for their other failures:
`GET /api/v1/agents/{id}/tasks` with 503 and an ERROR line that holds the error, the task detail and cancel with 409
(`errkind` classifies the error as `FailedPrecondition`), and the log stream with 503 and a WARN line that holds it.
Without a secret, only the network or client certificates keep other hosts
from enrolling new IDs (see `internal/config`, "Agent authentication" and "Enrollment secret"). A token over 1024 bytes
(`model.AgentTokenMaxBytes`) is refused first, with HTTP 400
`Agent token is too long.` / gRPC `InvalidArgument`, and counts as no failure; `VerifyOrEnrollToken` never enrolls one
either, while stored credentials load whatever their length. Then each sync reserves an attempt on
the pair (client IP, agent ID) before its token check (`verifyAgentToken`), so checks in flight count like failures; a
missing or wrong token turns the attempt into a failure, any other outcome releases it. A missing token is also logged
at WARN with the agent ID and the ways out; with an enrollment secret they name `podium agent-token`, not
`agent_auth.require=false`. Only attempts the limiter admits log, so each WARN, the one of a derived credential podium
cannot use included, comes at most `auth.rate_attempts` times
per `auth.rate_window` for a pair. Both transports share one limiter, so failures over HTTP count toward gRPC
and back. `auth.rate_attempts` failures block that pair for `auth.rate_window`: HTTP answers 429, gRPC `ResourceExhausted`.
There is no per-IP limit, so agents behind one egress IP (NAT, Kubernetes) never block each other.
HTTP takes the client IP from `trusted_proxies`, and in raft mode from the hosts of the raft configuration too. gRPC takes it
from the `x-forwarded-for` metadata of those hosts in raft mode, where a follower forwards a sync to the leader with the
agent's address in it (`grpcClientIP`), and uses the peer address in standalone.

A sync whose token check or upsert fails on the store is answered 503 / with the error's gRPC code, and logged at ERROR,
but for a leadership error (`cluster.IsLeadershipError`): a leader that lost its leadership while it served the sync,
or a deposed leader whose check of the leadership failed. That is a routing condition, as for the API's writes: it is
logged at WARN, and answered with the leader's address when one is known, over HTTP a 503 with `X-Leader`
(`response.StoreFailed`), over gRPC an `Unavailable` with the `x-leader` hint and the mark that makes `UnaryLogger` log
the call at WARN too, once (`interceptor.HintLeader`; `TestDiscoveryAnswersLeadershipErrorsWithTheLeader`).

### Sync rate

Each agent ID has a bucket of 10 syncs, refilled one per second (`NewSyncBuckets`; the numbers are constants, with no
configuration keys). It is used twice, for each sync that passed the payload checks. `syncOverRate` looks at it before the
token check, without taking from it: a sync that finds it empty is refused at once, whatever token it carries, so a
flood of syncs for a throttled agent opens no transaction (`TestThrottledAgentIsRefusedBeforeTheTokenCheck`,
`TestLimitedSyncWritesNothing`) and makes no failure for the failure limiter. `chargeSync` takes the token after the token
check, before the agent is written, so a sync with a wrong token, no token or an invalid payload takes nothing from the
bucket of the agent whose ID it carries (`TestWrongTokenDoesNotDrainTheBucket`), and the sync that enrolls an ID is charged
one token like any other (`TestEnrollingSyncIsChargedOneToken`). The look can race a concurrent sync, so a sync that found
a token may be refused at the charge, with nothing written (`TestSyncThatPassedThePeekMayBeRefusedAtTheCharge`); thirty
concurrent syncs of one agent land 10 all the same (`TestSyncBurstIsLimitedPerAgent`). A sync over the rate is answered
429 with `Retry-After: 1` over HTTP (`response.TooManyRequests`) or `ResourceExhausted` over gRPC, and writes nothing,
though the body read and the validation came first. `HTTPDiscovery` and `GRPCDiscovery` take the same buckets
(`TestDiscoveryTransportsShareTheSyncBudget`), so an agent has one budget whichever transport it uses, and with
`agent_auth.require=false` the rate still applies.

Refusing before the token check shows the throttling state of an agent ID to a caller that has not authenticated: a
wrong token for an agent whose bucket is empty gets 429, and one for an agent with tokens left gets 401 and takes nothing
from the bucket. Nobody can drain a bucket that way, since only a sync that passed the token check takes a token. The
WARN of the next paragraph can be caused by such a caller too: it names the agent ID, not who sent the sync.

The refusal logs one WARN with the agent ID, the first time for an ID and then at most once a minute for it, whichever of
the two steps refused; the log never holds a token. The access loggers record each refused call at DEBUG, one line each,
instead of the WARN of a 4xx or the ERROR of a failed gRPC call. That is the level of the lines, not their number:
`log.level` defaults to `info`, which drops those lines; at `debug` a DEBUG line is written like any other, and an agent
that keeps syncing writes one for each refused sync (`TestThrottledSyncsAreLoggedAtDebug` counts them).
The 429 carries `Retry-After` (`response.TooManyRequests`), the gRPC status carries `RetryInfo` (`status.Throttled`), and
`Logger` and `UnaryLogger` take either for a throttle, whichever node wrote it, so a follower that forwards a sync logs
the leader's refusal at DEBUG too (`TestAccessLogOfAForwardedThrottleIsDebug`, `TestUnaryLoggerLogsAThrottleAtDebug`).
The other refusals keep their levels: a wrong token, and the 429 / `ResourceExhausted` of the failure limiter.

The buckets are in memory, on the node that runs the handlers: in raft mode the leader, where a follower forwards a
sync, so the new leader counts from its own after a failover, and a restart clears them. A bucket that was not used for 10
minutes is dropped, and a look at a bucket is no use of it. The handlers read the time from a `clock` field that the
tests replace, so a refill is never waited for.

## UI pages
| Path                | Handler          | Auth | Permission  |
|---------------------|------------------|------|-------------|
| `/login`            | `UI.Login`       | —    | —           |
| `/logout` (POST)    | `UI.Logout`      | opt. | —           |
| `/`                 | `UI.Main`        | yes  | —           |
| `/users`            | `UI.Users`       | yes  | —           |
| `/users/info/{id}`  | `UI.UserDetail`  | yes  | `UsersGet`  |
| `/agents`           | `UI.Agents`      | yes  | —           |
| `/agents/info/{id}` | `UI.AgentDetail` | yes  | `AgentsGet` |
| `/history`          | `UI.History`     | yes  | `AgentsGet` |
| `/specs`            | `UI.Specs`       | yes  | `SpecsGet`  |
| `/specs/new`        | `UI.SpecNew`     | yes  | `SpecsAdd`  |
| `/specs/edit/{id}`  | `UI.SpecEdit`    | yes  | `SpecsEdit` |
| `/specs/info/{id}`  | `UI.SpecDetail`  | yes  | `SpecsGet`  |

`/logout` answers only POST (GET is 405). It authenticates like the other pages but also admits a caller without
valid credentials. It revokes the caller's own session, never the one the `session_id` cookie names, clears the auth
cookies and redirects to `/login` (303); without valid credentials it only clears the cookies. When the credentials
cannot be checked (a storage failure in the access check or the refresh) or the revoke fails, it answers 503 and leaves
the cookies untouched, so nobody is told they logged out while their session lives on; either failure is logged at
ERROR.

## Response pattern
All handlers detect render mode via `httpctx.ModeFromRequest(r)`:
- **JSON** — returns `responder.View.Data` as JSON
- **HTML** — renders `responder.View.Component` (templ)

HTMX updates are coordinated via `trigger.Set(w, event)` headers.
