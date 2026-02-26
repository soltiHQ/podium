# internal/transport
Transport layer — protocol-specific infrastructure that is placed between the network and the domain handlers.  
gRPC and HTTP share a common context layer (`httpctx`, `transportctx`) so handlers stay protocol-agnostic where possible.

## Package map
```text
transport/
├── grpc/
│   ├── interceptor/   unary server interceptors (auth, requestID, logger, recovery)
│   └── status/        domain-error → gRPC-code mapping + requestID detail attachment
│
├── http/
│   ├── middleware/     HTTP middleware pipeline (auth, negotiate, requestID, logger, recovery, CORS)
│   ├── responder/     Responder interface + HTML / JSON implementations
│   ├── response/      one-call status helpers (OK, NotFound, Unauthorized …)
│   ├── route/         middleware chaining helpers (BaseMW, PermMW, Chain)
│   ├── apimap/v1/     domain model → REST v1 DTO mappers
│   ├── cookie/        auth cookie management (set / delete / read)
│   └── ratelimitkey/  composite key builder for login rate-limiting
│
└── httpctx/           HTTP-specific request context (Responder, RenderMode)
```

## Request lifecycle
### HTTP
```text
  Browser / API client
        │
        ▼
  ┌─ middleware chain ───────────────────────────────────┐
  │  RequestID → Auth → Negotiate → Logger → Recovery    │
  └──────────────────────────────────────────────────────┘
        │
        │  context carries: requestID, identity, responder, renderMode
        ▼
  handler/api.go  or  handler/ui.go
        │
        ├─ response.OK(w, r, mode, &View{Data: dto, Component: tmpl})
        │       │                          │              │
        │       ▼                          ▼              ▼
        │   responder from ctx       JSON path       HTML path
        │       │
        │       ├── JSONResponder  →  json.Marshal(dto) + security headers
        │       └── HTMLResponder  →  templ.Render(tmpl) + CSP headers
        │
        └─ on error: response.NotFound / Unauthorized / …
                 │
                 └─ same dual-format pattern (JSON body + error page)
```

### gRPC
```text
  gRPC client
        │
        ▼
  ┌─ interceptor chain ──────────────────────────────────┐
  │  UnaryRequestID → UnaryAuth → UnaryLogger → Recovery │
  └──────────────────────────────────────────────────────┘
        │
        │  context carries: requestID, identity
        ▼
  handler/discovery.go (GRPCDiscovery)
        │
        ├─ success → proto response
        └─ error   → status.FromError(ctx, err)  or  status.Errorf(ctx, code, msg)
                          │
                          └─ maps domain errors to gRPC codes
                             attaches requestID as errdetails.RequestInfo
```

## Format negotiation (HTTP)
The `Negotiate` middleware decides **who renders** and **how**:
```text
  Request path          Responder       RenderMode
  ──────────────        ──────────      ──────────
  /api/v1/*             JSONResponder   (ignored)
  /* + HX-Request       HTMLResponder   RenderBlock  (HTMX fragment)
  /*                    HTMLResponder   RenderPage   (full page)
```

Handlers read `httpctx.Mode(ctx)` to pick between a full page layout
and a standalone HTMX fragment. The selected `Responder` is stored in
context; `response.*` helpers pull it out and call `Respond()`.

## Error mapping (gRPC)
`grpc/status` converts domain sentinel errors to appropriate gRPC codes:
```text
  Domain error                  →  gRPC code
  ────────────                     ─────────
  auth.ErrInvalidCredentials    →  Unauthenticated
  auth.ErrUnauthorized          →  PermissionDenied
  auth.ErrInvalidRequest        →  InvalidArgument
  storage.ErrNotFound           →  NotFound
  storage.ErrAlreadyExists      →  AlreadyExists
  storage.ErrConflict           →  Aborted
  context.Canceled              →  Canceled
  context.DeadlineExceeded      →  DeadlineExceeded
  (anything else)               →  Internal
```

Every error response includes `requestID` as `errdetails.RequestInfo`
for client-side log correlation.

## DTO mapping (HTTP)
`apimap/v1` contains pure functions that convert domain models into
versioned REST DTOs (`api/rest/v1`):
```text
  model.Agent       →  Agent(a)         →  restv1.Agent
  model.Spec        →  Spec(ts)         →  restv1.Spec
  model.Rollout     →  RolloutEntry(ss) →  restv1.RolloutEntry
  (Spec + states)   →  RolloutSpec(…)   →  restv1.RolloutSpec
  model.User        →  User(u)          →  restv1.User
  model.Role        →  Role(r)          →  restv1.Role
  model.Session     →  Session(s)       →  restv1.Session
```
