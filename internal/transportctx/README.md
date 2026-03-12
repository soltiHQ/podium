# internal/transportctx

Transport-agnostic context values are shared across HTTP and gRPC layers.
  
The package owns three typed context keys — **Identity**, **RequestID**, and **ErrorSlot** — and provides getters/setters for each.
Because the keys are unexported structs, no other package can collide with them.

## What goes into context
| Value                 | Writer                                               | Reader                                       |
|-----------------------|------------------------------------------------------|----------------------------------------------|
| `*identity.Identity`  | Auth middleware / interceptor                        | Handlers, loggers, permission checks         |
| `string` (request ID) | RequestID middleware / interceptor                   | Loggers, error responders                    |
| `*errorHolder` (slot) | RequestID middleware / interceptor (`WithErrorSlot`) | Logger middleware / interceptor (`TryError`) |

### Error slot
A mutable `errorHolder` pointer stored in context so that error response helpers can write a reason **after** the logger middleware has already captured the context.

- **Init**: `WithErrorSlot(ctx)` — called by RequestID middleware/interceptor before handler runs.
- **Write**: `SetError(ctx, msg)` — called by `response.*` (HTTP) and `status.*` (gRPC) helpers. No-op if slot was not initialised.
- **Read**: `TryError(ctx)` — called by Logger middleware/interceptor to append `"error"` field to the log line.

## Request lifecycle

```text
  incoming request (HTTP or gRPC)
    │
    ▼
  ┌──────────────────────────────┐
  │  RequestID middleware        │  WithRequestID(ctx, rid)
  │  ── extract or generate ──   │  WithErrorSlot(ctx)
  └──────────────┬───────────────┘
                 │
                 ▼
  ┌──────────────────────────────┐
  │  Auth middleware             │  WithIdentity(ctx, id)
  │  ── verify token ──          │
  └──────────────┬───────────────┘
                 │
                 ▼
  ┌──────────────────────────────┐
  │  Handler / Interceptor       │  Identity(ctx), RequestID(ctx)
  │  ── business logic ──        │  SetError(ctx, msg) via response/status helpers
  └──────────────┬───────────────┘
                 │
                 ▼
  ┌──────────────────────────────┐
  │  Logger middleware           │  TryError(ctx) → "error" field in log
  │  ── log request ──           │
  └──────────────────────────────┘
```

## Why a separate package
HTTP middleware lives in `internal/transport/http/middleware`, gRPC interceptors in `internal/transport/grpc/interceptors`.
Both need to write and read the same context values.  
Putting the keys here avoids a circular import:
```text
  transport/http/middleware ──┐
                              ├──→ transportctx ←── handler/api.go
  transport/grpc/interceptors ┘                 ←── handler/ui.go
                                                ←── loggers, responders
```
