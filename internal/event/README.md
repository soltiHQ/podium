# internal/event
Event broadcast hub with ring-buffered activity recording and Server-Sent Events streaming.
Handlers and runners record domain events and push real-time UI notifications through this package.

## Package map
```text
event/
├── event.go   Payload, Record, event kind constants, IsIssueKind()
├── hub.go     Hub — fan-out to SSE clients, dual ring buffers (events + issues)
├── ring.go    Ring[T] — generic thread-safe capped ring buffer
└── sse.go     Hub.SSEHandler() — text/event-stream HTTP endpoint
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
Created explicitly in `cmd/main.go` and injected into all consumers (no singleton).
```go
hub := event.NewHub(logger)
defer hub.Close()
```

| Method                      | Purpose                                                      |
|-----------------------------|--------------------------------------------------------------|
| `Notify(event)`             | Fan-out event name to every connected SSE client             |
| `Record(kind, payload)`     | Append to events ring; also to issues ring if `IsIssueKind`  |
| `Subscribe(ctx)`            | Register a new SSE listener, auto-unregister on ctx cancel   |
| `RecentEvents(n)`           | Last n activity events (reverse chronological)               |
| `RecentIssues(n)`           | Last n issue events (reverse chronological)                  |
| `DeleteIssues(kind, id)`    | Remove matching issues from the ring, return count           |
| `SSEHandler()`              | `http.HandlerFunc` that streams notifications                |
| `Close()`                   | Disconnect all clients, mark hub as closed                   |

Slow clients (full channel buffer) get their event dropped with a warning log instead of blocking the hub.

## Ring[T]
Generic capped ring buffer, thread-safe via `sync.RWMutex`.
Default capacity: 100 entries per buffer.

| Method               | Purpose                                   |
|----------------------|-------------------------------------------|
| `Append(item)`       | Add item, evict oldest when full          |
| `Recent(n)`          | Last n items in reverse chronological     |
| `DeleteFunc(match)`  | Remove matching items, return count       |
