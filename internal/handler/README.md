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
| `HTTPDiscovery`   | HTTP      | `NewHTTPDiscovery`    | agent service                                                        |
| `GRPCDiscovery`   | gRPC      | `NewGRPCDiscovery`    | agent service                                                        |
| `UI`              | HTTP      | `NewUI`               | access service                                                       |
| `Static`          | HTTP      | `NewStatic`           | embedded `ui.Static` filesystem                                      |

All constructors panic on nil dependencies — fail-fast at startup.

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

### Users `/api/v1/users`
| Method | Path                         | Permission    |
|--------|------------------------------|---------------|
| GET    | `/api/v1/users`              | `UsersGet`    |
| POST   | `/api/v1/users`              | `UsersAdd`    |
| GET    | `/api/v1/users/{id}`         | `UsersGet`    |
| PUT    | `/api/v1/users/{id}`         | `UsersEdit`   |
| DELETE | `/api/v1/users/{id}`         | `UsersDelete` |
| GET    | `/api/v1/users/{id}/sessions`| `UsersGet`    |
| POST   | `/api/v1/users/{id}/disable` | `UsersEdit`   |
| POST   | `/api/v1/users/{id}/enable`  | `UsersEdit`   |
| POST   | `/api/v1/users/{id}/password`| `UsersEdit`   |

### Sessions `/api/v1/sessions`
| Method | Path                                 | Permission    |
|--------|--------------------------------------|---------------|
| POST   | `/api/v1/sessions/{id}/revoke`       | `UsersEdit`   |

### Agents `/api/v1/agents`
| Method | Path                          | Permission    |
|--------|-------------------------------|---------------|
| GET    | `/api/v1/agents`              | `AgentsGet`   |
| GET    | `/api/v1/agents/{id}`         | `AgentsGet`   |
| PUT    | `/api/v1/agents/{id}/labels`  | `AgentsEdit`  |
| GET    | `/api/v1/agents/{id}/tasks`   | `AgentsGet`   |

### Specs `/api/v1/specs`
| Method | Path                         | Permission    |
|--------|------------------------------|---------------|
| GET    | `/api/v1/specs`              | `SpecsGet`    |
| POST   | `/api/v1/specs`              | `SpecsAdd`    |
| GET    | `/api/v1/specs/{id}`         | `SpecsGet`    |
| PUT    | `/api/v1/specs/{id}`         | `SpecsEdit`   |
| DELETE | `/api/v1/specs/{id}`         | `SpecsEdit`   |
| POST   | `/api/v1/specs/{id}/deploy`  | `SpecsDeploy` |
| GET    | `/api/v1/specs/{id}/sync`    | `SpecsGet`    |

### Other
| Method | Path                  | Permission    |
|--------|-----------------------|---------------|
| GET    | `/api/v1/permissions` | `UsersEdit`   |
| GET    | `/api/v1/roles`       | `UsersEdit`   |

## Discovery endpoint
| Transport | Method             | Path / RPC                     |
|-----------|--------------------|--------------------------------|
| HTTP      | POST               | `/api/v1/discovery/sync`       |
| gRPC      | `DiscoverService/Sync` | proto-defined                |

Both parse the agent heartbeat payload, call `model.NewAgentFrom{Sync,Proto}`, then `agentSVC.Upsert`.

## UI pages
| Path               | Handler          | Auth | Permission |
|--------------------|------------------|------|------------|
| `/login`           | `UI.Login`       | —    | —          |
| `/logout`          | `UI.Logout`      | yes  | —          |
| `/`                | `UI.Main`        | yes  | —          |
| `/users`           | `UI.Users`       | yes  | —          |
| `/users/info/{id}` | `UI.UserDetail`  | yes  | `UsersGet` |
| `/agents`          | `UI.Agents`      | yes  | —          |
| `/agents/info/{id}`| `UI.AgentDetail` | yes  | `AgentsGet`|
| `/specs`           | `UI.Specs`       | yes  | `SpecsGet` |
| `/specs/new`       | `UI.SpecNew`     | yes  | `SpecsAdd` |
| `/specs/info/{id}` | `UI.SpecDetail`  | yes  | `SpecsGet` |

## Response pattern
All handlers detect render mode via `httpctx.ModeFromRequest(r)`:
- **JSON** — returns `responder.View.Data` as JSON
- **HTML** — renders `responder.View.Component` (templ)

HTMX updates are coordinated via `trigger.Set(w, event)` headers.
