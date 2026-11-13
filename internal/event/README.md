# internal/event

Event broadcast hub with ring-buffered activity recording and Server-Sent Events
streaming. Handlers and runners record domain events and push real-time UI
notifications through this package.

## Package map

```text
event/
├── event.go   Payload, Record, event kind constants, IsIssueKind(), IsRefreshKind(), SubjectOf()
├── config.go  Config — the events.coalesce window (YAML / env / default / validation)
├── hub.go     Hub — fan-out to SSE clients, coalescing of refresh pulses, dual ring buffers (events + issues)
├── ring.go    Ring[T] — generic thread-safe capped ring buffer
└── sse.go     Hub.SSEHandler(access) — text/event-stream HTTP endpoint
```

## Event flow

```text
  mutation (handler / runner)
          │
          ├─ hub.Record(kind, payload)     append to ring buffer (dashboard feed)
          └─ hub.Notify(event)             broadcast to all SSE clients
                  │
                  ▼
  ┌───────────────── SSE channel per browser tab ────────────┐
  │  EventSource → htmx.trigger(document.body, event)        │
  └──────────────────────────────────────────────────────────┘
          │
          ▼
  hx-trigger="… event from:body"    Results div refetches
```

## Hub

Created explicitly in `internal/app` (`buildCluster`, with the `events.coalesce`
window) and injected into all consumers (no singleton); the app closes it as the
UI listener starts to stop, and in `App.Shutdown`.

```go
hub := event.NewHub(logger, event.WithCoalesce(time.Second))
defer hub.Close()
```

| Method | Purpose |
| -- | -- |
| `Notify(event)` | Fan-out event name to every connected SSE client (refresh pulses coalesced, see below) |
| `Record(kind, payload)` | Append to events ring; also to issues ring if `IsIssueKind` |
| `Subscribe(ctx)` | Register a new SSE listener, auto-unregister on ctx cancel |
| `RecentEvents(n)` | Last n activity events (reverse chronological) |
| `RecentIssues(n)` | Last n issue events (reverse chronological) |
| `DeleteIssues(kind, id)` | Remove matching issues from the ring, return count |
| `SSEHandler(access)` | Stream notifications; end when the `access` re-check fails |
| `Close()` | Disconnect all clients, mark hub as closed |

Slow clients (full channel buffer) get their event dropped with a warning log
instead of blocking the hub.

## Coalescing

A refresh pulse (`IsRefreshKind`: `agent_update`, `spec_update`,
`dashboard_update`, `user_update`, `session_update`) only tells a page to fetch
again, so a burst of them need not reach the browsers one by one.
`WithCoalesce(window)` (the app passes `events.coalesce`, default 1s) delivers
each kind to the clients at most once per window:

- the first pulse after a quiet period is delivered at once;
- the pulses that follow within the window become one trailing delivery at its
  end, so a burst costs two deliveries and its last change is never lost;
- under steady load that is one delivery per window and kind;
- any other event, and every event of a hub built without a window, is delivered
  as it comes.

The window holds pulses back in `ApplyLocalNotify`, where a pulse reaches the
clients of a replica: from a `Notify` of the memory backend, from the state
machine of every Raft replica that applies a replicated pulse, and from the
local fallback of a follower. Each replica thus bounds the traffic to its own
clients whichever node sent the burst, and delivers its trailing pulse from what
it applied itself, also after leadership moved. `Notify` still submits one
command per pulse to the `Writer`: coalescing does not reduce replication
traffic.

The trailing delivery runs on a timer, never on the caller; it only writes to
local channels without blocking, so no timer calls into the replication
transport. `Close` stops the timers and a closed hub schedules none. The
keepalive and the access re-check of `SSEHandler` run on their own tickers and
are not events: the window never delays them.

## Ring[T]

Generic capped ring buffer, thread-safe via `sync.RWMutex`. Default capacity:
100 entries per buffer.

| Method | Purpose |
| -- | -- |
| `Append(item)` | Add item, evict oldest when full |
| `Recent(n)` | Last n items in reverse chronological |
| `DeleteFunc(match)` | Remove matching items, return count |
