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
| `HTTPDiscovery`   | HTTP      | `NewHTTPDiscovery`    | agent service, discovery limiter, client IP resolver                 |
| `GRPCDiscovery`   | gRPC      | `NewGRPCDiscovery`    | agent service, discovery limiter, client IP resolver (raft mode)     |
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
(see disable and enable). A present `role_ids` or `permissions` list overwrites the stored set, and the UI form for
editing another user always sends both, so a stale form can still revert a concurrent role or permission change
(known residual).

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
| Method | Path                                             | Permission   |
|--------|--------------------------------------------------|--------------|
| GET    | `/api/v1/agents`                                 | `AgentsGet`  |
| GET    | `/api/v1/agents/{id}`                            | `AgentsGet`  |
| PUT    | `/api/v1/agents/{id}/labels`                     | `AgentsEdit` |
| GET    | `/api/v1/agents/{id}/tasks`                      | `AgentsGet`  |
| GET    | `/api/v1/agents/{id}/tasks/{taskID}`             | `AgentsGet`  |
| POST   | `/api/v1/agents/{id}/tasks/{taskID}/cancel`      | `AgentsEdit` |
| GET    | `/api/v1/agents/{id}/tasks/{taskID}/logs/stream` | `AgentsGet`  |
| GET    | `/api/v1/history`                                | `AgentsGet`  |

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

| Subject | Event kinds                                                                | Read        | Dismiss      |
|---------|----------------------------------------------------------------------------|-------------|--------------|
| agent   | `agent_connected`, `agent_inactive`, `agent_disconnected`, `agent_deleted` | `AgentsGet` | `AgentsEdit` |
| spec    | `spec_created`, `spec_updated`, `spec_deployed`, `sync_failed`             | `SpecsGet`  | `SpecsEdit`  |
| user    | `user_*`, `session_created`, `rate_limited`                                | `UsersGet`  | `UsersEdit`  |
| unknown | `issue_closed` (it can name any subject) and unknown kinds                 | all three   | all three    |

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
the configuration writes it, and its `suffrage` (`voter`, `nonvoter` or `staging`), and `last_index`, the index of the last
entry of its log (`TestClusterStatusAnswer`).

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
| 503    | the write met a follower or lost leadership: with `X-Leader`, as every write (see "Leader hint" in `internal/transport/README.md`); the leader's own removal, below |

The leader asked to remove itself first hands its leadership over to another voter, with hashicorp/raft's leadership
transfer, so that writes resume without an election timeout, and removes nothing yet. Once it knows the new leader, it
answers 503 with `X-Leader` and the message `This node handed its leadership over and is still a member: repeat the
request, at the leader that X-Leader names, to remove it.`. The request repeated there, or at the same node, which now
forwards it, removes a follower. So the removal of the leader takes two calls. When no new leader is known after 5 s, or
none can be named, the 503 has no `X-Leader` and says `Leadership moved; no leader is known yet, retry.`. A transfer that
fails, as one to a dead voter that hashicorp/raft picked, removes nothing and answers 503 without `X-Leader`: remove dead
members first. See "Membership" in `internal/raft/README.md`, which also says how to replace a dead member, and what
becomes of a removed node, whose process must be stopped.

Every change that commits logs one INFO line, `cluster membership changed`, with `actor_user_id`, `operation` (`add` or
`remove`), `member_id`, `member_address` and `force`; a hand-over logs one too, with the `transfer` operation. A change
that lost leadership while it committed, which may still take effect, logs one WARN line with the same fields instead,
`cluster membership change outcome unknown`. No line names a token.

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

Both parse the agent heartbeat payload, call `model.NewAgentFrom`, then `agentSVC.Upsert`. The upsert reports what
the sync changed in the stored row (`agent.UpsertResult`), and discovery sends the `agent_update` refresh pulse only
when the sync gave the UI pages something new: a **visible change**, that is, a new agent or a different status, name,
endpoint, labels or capabilities, or a **reconnect**, that is, a new agent, an agent that was not active, or one that
reports less uptime than its stored row (`model.Agent.ReconnectedSince`, the rule that also re-arms failed rollouts).
A heartbeat that moves only the time of the sync and the uptime sends none. The hub coalesces the pulses it does send
(`events.coalesce`).

With agent auth required, the first sync of an unknown agent ID enrolls the token it presents (trust on first use) and
sets the endpoint podium calls; every later sync of that ID must present the same token. With an enrollment secret
(`SOLTI_AGENT_AUTH_ENROLL_SECRET`, held by the agent service as `agent.WithEnrollSecret`), the first sync enrolls only
the token derived for that ID (`agent.EnrollToken`, printed by `podium agent-token`): any other token is an
`ErrEnrollRefused`, an `ErrUnauthenticated` like a wrong one, so 401 / `Unauthenticated`, nothing stored, one failure for
the limiter, and a WARN with the agent ID and the client IP, never the token. Known IDs
are checked against their stored token, secret or not. Without a secret, only the network or client certificates keep
other hosts from enrolling new IDs (see `internal/config`, "Agent authentication"). A token over 1024 bytes
(`model.AgentTokenMaxBytes`) is refused first, with HTTP 400
`Agent token is too long.` / gRPC `InvalidArgument`, and counts as no failure; `VerifyOrEnrollToken` never enrolls one
either, while stored credentials load whatever their length. Then each sync reserves an attempt on
the pair (client IP, agent ID) before its token check (`verifyAgentToken`), so checks in flight count like failures; a
missing or wrong token turns the attempt into a failure, any other outcome releases it. A missing token is also logged
at WARN with the agent ID and the ways out; with an enrollment secret they name `podium agent-token`, not
`agent_auth.require=false`. Only attempts the limiter admits log, so each WARN comes at most `auth.rate_attempts` times
per `auth.rate_window` for a pair. Both transports share one limiter, so failures over HTTP count toward gRPC
and back. `auth.rate_attempts` failures block that pair for `auth.rate_window`: HTTP answers 429, gRPC `ResourceExhausted`.
There is no per-IP limit, so agents behind one egress IP (NAT, Kubernetes) never block each other.
HTTP takes the client IP from `trusted_proxies`, and in raft mode from the hosts of the raft configuration too. gRPC takes it
from the `x-forwarded-for` metadata of those hosts in raft mode, where a follower forwards a sync to the leader with the
agent's address in it (`grpcClientIP`), and uses the peer address in standalone.

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
