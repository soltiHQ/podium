# internal/uikit
Shared UI infrastructure consumed by templates, handlers, and the server entrypoint.
Everything here is internal to the control plane; agents never import it.

## Package map
```text
uikit/
├── policy/       permission-based UI visibility flags
├── routepath/    URL constants for pages and API endpoints
├── timeformat/   human-readable time formatting (relative, session, uptime)
└── htmx/         HTMX response helpers, polling intervals, event.Refresh* aliases
```

## policy
Template-oriented permission models.
Each `Build*` function takes an `identity.Identity` and returns a struct of bool flags that templ templates use to show/hide interactive elements.

```text
identity.Identity
       │
       ▼
  BuildNav(id)                                   → Nav          (sidebar: ShowUsers, ShowAgents, CanAddUser…)
  BuildUserDetail(id, targetUserID, manageable)  → UserDetail   (detail page: CanEdit, CanEditRoles, CanDelete,
                                                                  CanRevokeSessions + IsSelf guard and target ceiling)
  BuildAgentDetail(id)                           → AgentDetail  (detail page: CanEditLabels…)
  BuildSpecDetail(id)                            → SpecDetail   (detail page: CanEdit, CanDelete, CanDeploy…)
  BuildDashboard(id)                             → Dashboard    (issue list: CanDismiss(kind))
```

The flags mirror the API rules: another user's actions need every permission that user holds (`manageable` is the
user service's own decision), spec delete needs `taskspecs:delete`, and an issue is dismissed with the edit permission
of its subject. The user edit form's role and permission options come from the API, which lists only what the viewer
may grant.

## routepath
All URL constants in one place — pages (`Page*`) and API endpoints (`Api*`).
Path-builder functions append entity IDs, each escaped as one path segment, so an ID can never add a query or a fragment, or break out of the HTML attribute or JavaScript string the URL ends up in:

```go
routepath.PageUserInfoByID("abc")  // → "/users/info/abc"
routepath.ApiAgentTasks("xyz")     // → "/api/v1/agents/xyz/tasks"
routepath.ApiAgentLabels("a/b c")  // → "/api/v1/agents/a%2Fb%20c/labels"
```

Handlers route on the decoded `r.URL.Path`, where an escaped `/` is a separator again, so an ID that contains `/` is not supported; agent IDs and ksuids cannot contain one. Any other ID reaches its handler unchanged. Never build an entity URL with `+ id`; add a helper here.

## timeformat
Human-readable time formatting helpers used by templ templates.

Each helper degrades gracefully on missing data (zero time, negative duration) by rendering an em dash, so templates never show garbage:
```text
  timeformat.Relative(t)   →  "—", "just now", "5m ago", "2h ago", "3d ago"
  timeformat.Session(t)    →  "—", "Jan 02, 15:04" (current year) / "Jan 02 2006, 15:04"
  timeformat.Uptime(secs)  →  "—", "0s", "30s", "5m", "2h 15m", "3d 4h"
```

## htmx
HTMX response helpers and configurable polling intervals.
Event infrastructure (Hub, ring buffers, SSE) lives in `internal/event`.

### Helpers
```go
htmx.Trigger(w, htmx.UserUpdate)                                 // set HX-Trigger header
htmx.Redirect(w, routepath.PageUsers)                            // set HX-Redirect header
htmx.Poll(htmx.Every1m, htmx.AgentUpdate)                        // "every 60s, agent_update from:body"
htmx.PollMulti(htmx.Every3m, htmx.AgentUpdate, htmx.SpecUpdate)
htmx.LoadAndPoll(htmx.Every1m, htmx.SpecUpdate)                  // "load, every 60s, spec_update from:body"
```

### Trigger names (aliases of `event.Refresh*`)
| htmx alias (templates) | canonical (`internal/event`) | wire value         |
|------------------------|------------------------------|--------------------|
| `SessionUpdate`        | `event.RefreshSessions`      | `session_update`   |
| `AgentUpdate`          | `event.RefreshAgents`        | `agent_update`     |
| `SpecUpdate`           | `event.RefreshSpecs`         | `spec_update`      |
| `UserUpdate`           | `event.RefreshUsers`         | `user_update`      |
| `DashboardUpdate`      | `event.RefreshDashboard`     | `dashboard_update` |

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

Intervals are overridable via `htmx.Configure(Config{…})` at startup.

### Refresh architecture
SSE/polling triggers live on `Results` containers, not on the outer page loader.
This keeps the search input untouched during a refresh cycle:

```text
  HTMXLoader (trigger="load")          <- one-time initial fetch
       |
       v
  +- List ------------------------------------+
  |  SearchInput  <- stays in DOM             |
  |                                           |
  |  #results  (hx-trigger="every 60s,        | <- handles SSE + polling
  |     agent_update from:body"               |
  |     hx-include="#search-input"            | <- preserves search query
  |     hx-swap="outerHTML"                   |
  |     hx-select="#results")                 | <- picks only results from response
  +-------------------------------------------+
```
