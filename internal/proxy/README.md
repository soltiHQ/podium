# internal/proxy
Outbound communication with agents.
The control plane calls INTO agents to list tasks, submit specs, etc.

## Package map
```text
proxy/
├── proxy.go        AgentProxy interface, request/response DTOs
├── pool.go         Pool — connection manager (HTTP transport + gRPC conn cache)
├── httpclient.go   httpClient interface, doGet[T] / doPost helpers
├── v1_http.go      httpProxyV1 — AgentProxy over HTTP (API v1)
├── v1_grpc.go      grpcProxyV1 — AgentProxy over gRPC (API v1, partial)
└── error.go        sentinel errors
```

## Request flow
```text
  sync runner / handler
        │
        ▼
  Pool.Get(endpoint, type, version)
        │
   ┌────┴────────────────┐
   │ HTTP                │ gRPC
   │ httpProxyV1{        │ grpcProxyV1{
   │   endpoint, client  │   conn (cached)
   │ }                   │ }
   └────┬────────────────┘
        │
        ▼
  AgentProxy.SubmitTask / ListTasks
        │
   ┌────┴────────────────┐
   │ doPost / doGet[T]   │ genv1.SoltiApiClient
   │ (httpclient.go)     │ (proto-generated)
   └─────────────────────┘
```

## Pool
```text
  Pool
  ├── httpCli    *http.Client              shared, Transport pools TCP connections
  └── grpcConns  map[endpoint]*ClientConn  one conn per endpoint, double-check lock
```
- `Get(endpoint, type, version)` dispatches to versioned factory (`getV1`)
- `Close()` drains HTTP idle conns + closes all gRPC conns

## AgentProxy interface
```go
type AgentProxy interface {
    ListTasks(ctx, filter)      → (*TaskListResponse, error)
    SubmitTask(ctx, submission) → error
}
```

## API v1 support matrix

| Method       | HTTP | gRPC |
|--------------|------|------|
| `ListTasks`  | ✓    | ✓    |
| `SubmitTask` | ✓    | —    |

gRPC stubs return `ErrSubmitTask`: proto does not yet define the RPC.

## HTTP helpers (httpclient.go)
| Helper       | Purpose                                            |
|--------------|----------------------------------------------------|
| `doGet[T]`   | GET + JSON decode into `*T`                        |
| `doPost`     | POST JSON body, accept 200 / 201 / 204             |

Both use `httpClient` interface (`Do` method) for testability.
Timeouts are controlled by the caller's `ctx`, not hardcoded.
