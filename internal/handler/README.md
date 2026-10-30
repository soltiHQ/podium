# internal/handler
HTTP and gRPC request handlers for the control plane.

## Package map
```text
handler/
├── handler.go      package documentation
├── api.go          API — REST + HTMX endpoints (users, agents, specs, sessions, roles)
├── discovery.go    HTTPDiscovery + GRPCDiscovery — agent heartbeat / sync
├── ui.go           UI — full-page HTML renders (login, dashboard, detail pages)
└── static.go       Static — embedded file serving (CSS, JS, images)
```

## Handlers

| Handler           | Transport | Constructor           | Dependencies                                                         |
|-------------------|-----------|-----------------------|----------------------------------------------------------------------|
| `API`             | HTTP      | `NewAPI`              | user, access, session, credential, agent, spec services + proxy.Pool |
| `HTTPDiscovery`   | HTTP      | `NewHTTPDiscovery`    | agent service, discovery limiter, client IP resolver                 |
| `GRPCDiscovery`   | gRPC      | `NewGRPCDiscovery`    | agent service, discovery limiter                                     |
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

Endpoints that read a JSON body (user create, update and password, agent labels, spec create and update) require
`Content-Type: application/json`, parameters allowed; any other type gets 415 `Content-Type must be
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
generation), so neither a heartbeat, a sync nor a completed uninstall changes it. A spec save does, through the
version, even when generation stays, and so does another deploy that created rollouts.

Deploy takes the token as `If-Match`, one strong entity tag with or without its quotes. When the plan recomputed in
the deploy transaction has another token, Deploy writes nothing and answers 412 `Placement changed since it was
previewed. Review it again.`; a blank `If-Match` names no plan and gets the same answer. Only without `If-Match`, or
with `*`, Deploy deploys the current placement unchecked, as before, so API clients that never preview keep working.

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

Both parse the agent heartbeat payload, call `model.NewAgentFrom`, then `agentSVC.Upsert`.

With agent auth required, the first sync of an unknown agent ID enrolls the token it presents (trust on first use) and
sets the endpoint podium calls; every later sync of that ID must present the same token. With an enrollment secret
(`SOLTI_AGENT_AUTH_ENROLL_SECRET`, held by the agent service as `agent.WithEnrollSecret`), the first sync enrolls only
the token derived for that ID (`agent.EnrollToken`, printed by `podium agent-token`): any other token is an
`ErrUnauthenticated` like a wrong one, so 401 / `Unauthenticated`, nothing stored, one failure for the limiter. Known IDs
are checked against their stored token, secret or not. Without a secret, only the network or client certificates keep
other hosts from enrolling new IDs (see `internal/config`, "Agent authentication"). A token over 1024 bytes
(`model.AgentTokenMaxBytes`) is refused first, with HTTP 400
`Agent token is too long.` / gRPC `InvalidArgument`, and counts as no failure; `VerifyOrEnrollToken` never enrolls one
either, while stored credentials load whatever their length. Then each sync reserves an attempt on
the pair (client IP, agent ID) before its token check (`verifyAgentToken`), so checks in flight count like failures; a
missing or wrong token turns the attempt into a failure, any other outcome releases it. A missing token is also logged
at WARN with the agent ID and the ways out. Both transports share one limiter, so failures over HTTP count toward gRPC
and back. `auth.rate_attempts` failures block that pair for `auth.rate_window`: HTTP answers 429, gRPC `ResourceExhausted`.
There is no per-IP limit, so agents behind one egress IP (NAT, Kubernetes) never block each other.
HTTP takes the client IP from `trusted_proxies`; gRPC uses the peer address.

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
