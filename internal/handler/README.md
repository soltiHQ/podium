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
| `UI`              | HTTP      | `NewUI`               | access + spec services, client IP resolver                           |
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
├── UI.Routes(mux, auth, perm, common...)
│     perm(kind.Permission) added per-route at mux level
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
| POST   | `/api/v1/users/{id}/password` | `UsersEdit`                                              |

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

Both parse the agent heartbeat payload, call `model.NewAgentFrom{Sync,Proto}`, then `agentSVC.Upsert`.

With agent auth required, every token failure counts against the pair (client IP, agent ID); the limiter is checked
before the token. `auth.rate_attempts` failures block that pair for `auth.rate_window`: HTTP answers 429, gRPC
`ResourceExhausted`. There is no per-IP limit, so agents behind one egress IP (NAT, Kubernetes) never block each other.
HTTP takes the client IP from `trusted_proxies`; gRPC uses the peer address.

## UI pages
| Path                | Handler          | Auth | Permission  |
|---------------------|------------------|------|-------------|
| `/login`            | `UI.Login`       | —    | —           |
| `/logout`           | `UI.Logout`      | yes  | —           |
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

## Response pattern
All handlers detect render mode via `httpctx.ModeFromRequest(r)`:
- **JSON** — returns `responder.View.Data` as JSON
- **HTML** — renders `responder.View.Component` (templ)

HTMX updates are coordinated via `trigger.Set(w, event)` headers.
