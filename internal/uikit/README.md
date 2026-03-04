# internal/uikit
Shared UI infrastructure consumed by templates, handlers, and the server entrypoint.
Everything here is internal to the control plane; agents never import it.

## Package map
```text
uikit/
├── policy/       permission-based UI visibility flags
├── routepath/    URL constants for pages and API endpoints
└── trigger/      HTMX events, polling intervals, and SSE notification hub
```

## policy
Template-oriented permission models.
Each `Build*` function takes an `identity.Identity` and returns a struct of bool flags
that templ templates use to show/hide interactive elements.

```text
identity.Identity
       │
       ▼
  BuildNav(id)          → Nav          (sidebar: ShowUsers, ShowAgents, CanAddUser…)
  BuildUserDetail(id)   → UserDetail   (detail page: CanEdit, CanDelete, CanRevoke…)
  BuildAgentDetail(id)  → AgentDetail  (detail page: CanEditLabels…)
  BuildSpecDetail(id)   → SpecDetail   (detail page: CanEdit, CanDelete, CanDeploy…)
```

## routepath
All URL constants in one place — pages (`Page*`) and API endpoints (`Api*`).
Path-builder functions append entity IDs:

```go
routepath.PageUserInfoByID("abc")  // → "/users/info/abc"
routepath.ApiAgentTasks("xyz")     // → "/api/v1/agents/xyz/tasks"
```

## trigger
Real-time UI update pipeline: HTMX events + SSE broadcast hub.

### Event flow
```text
  mutation (handler / runner)
          │
          ├─ trigger.Set(w, event)     HX-Trigger header + SSE broadcast
          └─ trigger.Notify(event)     SSE broadcast only (no ResponseWriter)
          │
          ▼
     Hub.Notify(event)
          │
          ▼
  ┌── SSE channel per browser tab ──┐
  │  EventSource → htmx.trigger(    │
  │    document.body, event)        │
  └─────────────────────────────────┘
          │
          ▼
  hx-trigger="… event from:body"    Results div refetches
```

### File map
```text
trigger/
├── trigger.go   event constants, polling config, Set() / Redirect()
├── hub.go       Hub type — fan-out to subscribers, thread-safe
├── global.go    package-level singleton (InitHub, CloseHub, Notify, Subscribe)
└── sse.go       SSEHandler() — text/event-stream HTTP endpoint
```

### Polling intervals (defaults)
| Scope           | Interval | Getter                     |
|-----------------|----------|----------------------------|
| User list       | 3 min    | `GetUsersRefresh()`        |
| User detail     | 5 min    | `GetUserDetailRefresh()`   |
| User sessions   | 3 min    | `GetUserSessionsRefresh()` |
| Agent list      | 1 min    | `GetAgentsRefresh()`       |
| Agent detail    | 3 min    | `GetAgentDetailRefresh()`  |
| Agent tasks     | 1 min    | `GetAgentTasksRefresh()`   |
| Spec list       | 3 min    | `GetSpecsRefresh()`        |
| Spec detail     | 1 min    | `GetSpecDetailRefresh()`   |

Intervals are overridable via `trigger.Configure(Config{…})` at startup.

### Refresh architecture
SSE/polling triggers live on `Results` containers, not on the outer page loader.
This keeps the search input untouched during a refresh cycle:

```text
  HTMXLoader (trigger="load")          ← one-time initial fetch
       │
       ▼
  ┌─ List ──────────────────────────┐
  │  SearchInput  ← stays in DOM    │
  │                                 │
  │  #results  (hx-trigger="every   │ ← handles SSE + polling
  │     60s, agent_update from:body"│
  │     hx-include="#search-input") │ ← preserves search query
  │     hx-swap="outerHTML"         │
  │     hx-select="#results"        │ ← picks only results from response
  └─────────────────────────────────┘
```
