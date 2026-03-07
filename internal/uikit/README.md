# internal/uikit
Shared UI infrastructure consumed by templates, handlers, and the server entrypoint.
Everything here is internal to the control plane; agents never import it.

## Package map
```text
uikit/
├── policy/       permission-based UI visibility flags
├── routepath/    URL constants for pages and API endpoints
├── timeformat/   human-readable time formatting (relative, session, uptime)
└── trigger/      HTMX events, polling intervals, event recording, and SSE notification hub
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

## timeformat
Human-readable time formatting helpers used by templ templates.

```text
  timeformat.Relative(t)   →  "just now", "5m ago", "2h ago", "3d ago"
  timeformat.Session(t)    →  "Jan 02, 15:04" (current year) / "Jan 02 2006, 15:04"
  timeformat.Uptime(secs)  →  "30s", "5m", "2h 15m", "3d 4h"
```

## trigger
Real-time UI update pipeline: HTMX events + SSE broadcast hub + event recording.

### Event flow
```text
  mutation (handler / runner)
          │
          ├─ trigger.Set(w, event)       HX-Trigger header + SSE broadcast
          ├─ trigger.Notify(event)       SSE broadcast only (no ResponseWriter)
          └─ trigger.Record(kind, payload)  append to ring buffer for dashboard feed
          │
          ▼
     Hub.Notify(event)
          │
          ▼
  ┌───────────────── SSE channel per browser tab ────────────┐
  │  EventSource → htmx.trigger(document.body, event)        │
  └──────────────────────────────────────────────────────────┘
          │
          ▼
  hx-trigger="… event from:body"    Results div refetches
```

### Event kinds (dashboard feed)
```text
  agent_connected, agent_inactive, agent_disconnected, agent_deleted
  spec_created, spec_updated, spec_deployed
  user_created, user_updated, user_deleted, user_password_changed, user_status_changed
  session_created, rate_limited
```

### File map
```text
trigger/
├── trigger.go   event constants, polling config, Set() / Redirect()
├── hub.go       Hub type — fan-out, ring buffer (EventRecord, Record, RecentEvents)
├── global.go    package-level singleton (InitHub, CloseHub, Notify, Record, RecentEvents, Subscribe)
└── sse.go       SSEHandler() — text/event-stream HTTP endpoint
```

### Polling intervals (defaults)
| Scope           | Interval | Getter                     |
|-----------------|----------|----------------------------|
| Dashboard       | 1 min    | `GetDashboardRefresh()`    |
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
  ┌─ List ─────────────────────────────┐
  │  SearchInput  ← stays in DOM       │
  │                                    │
  │  #results  (hx-trigger="every 60s, │ ← handles SSE + polling
  │     agent_update from:body"        │
  │     hx-include="#search-input"     │ ← preserves search query
  │     hx-swap="outerHTML"            │
  │     hx-select="#results")          │ ← picks only results from response
  └────────────────────────────────────┘
```
