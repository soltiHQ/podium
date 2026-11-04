# internal/transport

Transport layer - protocol-specific wiring between the network and the domain handlers. 
HTTP and gRPC are kept symmetric where the protocols allow, and share two transport-agnostic packages so handlers stay protocol-neutral:
- `internal/transportctx` (sibling, outside this tree to avoid import cycles) – identity, request id, and the mutable error slot.
- `errkind` - the single error → category classifier consumed by both transports.

## Package map

```text
transport/
├── grpc/
│   ├── interceptor/   unary + stream interceptors
│   └── status/        Kind → gRPC code + requestID detail
│
├── http/
│   ├── middleware/    pipeline 
│   ├── responder/     Responder interface + JSON / HTML implementations
│   ├── response/      one-call helpers (OK, NotFound, …) + FromError
│   └── route/         middleware chaining + REST dispatch (Resource, Router)
│
├── httpctx/           HTTP-only request context (Responder, RenderMode, Leader)
├── clientip/          client IP + HTTPS detection behind trusted proxies
├── tlsconf/           PEM config → *tls.Config (server + client, mTLS)
└── errkind/           domain/agent error → Kind (shared by both transports)
```

## Request lifecycle

### HTTP

Main listener (`app.buildMainHandler`), outermost first:

```text
  Browser / API client
        │
        ▼
  Recovery       ── turns a panic into 503
   CORS          ── answers preflights, adds the CORS headers
    Leader       ── raft only: forwards writes to the leader, over TLS with tls.client when tls.server is set;
                    stores where the leader serves this listener in ctx (httpctx.Leader), for X-Leader
     Negotiate   ── picks the Responder (JSON vs HTML), stores it in ctx
      CrossOrigin ── refuses cross-origin browser mutations: 403 through that Responder.
                     It runs before any route, so a refusal gets no request id and no
                     access-log line; CrossOrigin logs it at WARN instead.
       mux
        │  per route:
  RequestID      ── installs request id + error slot (API routes and the event stream only)
   Logger        ── reads the slot back after the handler returns (same routes); logs 5xx at ERROR, but a 503 with X-Leader at WARN
    Auth / OptionalAuth / RequirePermission  ── 503 when credentials cannot be checked, logged at ERROR; a refresh that needs the
                                                 raft leader (auth.rotate_refresh on a follower) is 503 + X-Leader, logged at WARN
     handler/api.go | ui.go
        ├─ success → response.OK(w, r, mode, &View{Data: dto, Component: tmpl})
        │              └─ Responder from ctx → JSON (json.Marshal + sec headers)
        │                                    or HTML (templ.Render + CSP)
        └─ error   → response.FromError(w, r, mode, err)   ← classifies via errkind
                       ├─ cluster.ErrNotLeader / ErrLeadershipLost: 503 + X-Leader, see "Leader hint" below
                       └─ sets the error slot → Logger appends it to the log line
```

`RenderMode` (full page vs HTMX fragment) is a pure function of the request header, derived on demand via `httpctx.ModeFromRequest(r)`: 
only the negotiated `Responder` is stored in context.

### gRPC

```text
  gRPC client
        ▼
  UnaryRecovery → UnaryRequestID → UnaryLogger → UnaryLeader
        │   (UnaryAuth / UnaryRequirePermission exist but are not chained yet)
        ▼
  handler/discovery.go (GRPCDiscovery)
        │
        ├─ success → proto response
        └─ error   → status.FromError(ctx, err)            ← classifies via errkind
                       ├─ Kind → gRPC code + stable message
                       ├─ attaches requestID as errdetails.RequestInfo
                       └─ sets the error slot → UnaryLogger appends it
```

## Client IP

`clientip.Resolver` decides who the client of an HTTP request is (`trusted_proxies`, see `internal/config`):
- `IP(r)` - the TCP peer; when the peer is a trusted proxy, the right-most `X-Forwarded-For` entry that is not itself trusted.
- `HTTPS(r)` - direct TLS, or `X-Forwarded-Proto: https` from a trusted proxy. With `auth.cookie_secure: auto` it decides
  whether the auth and device cookies are `Secure`; `app` builds that decision once and hands it to every cookie writer.

RFC 7239 `Forwarded` is not supported. The login limiter, `middleware.StreamLimit` and HTTP discovery take the client IP from the resolver.
gRPC requests carry no forwarding headers: gRPC discovery uses the peer address.

## Rate limits

- The main listener has no limit on failed authentication: its tokens and session IDs cannot be guessed, and passwords go through the login limiter (`internal/auth`).
- Discovery has no per-IP limit. Both Sync handlers reserve an attempt per (client IP, agent ID) before the token check, in one shared limiter, so checks in flight and failures count
  together and across HTTP and gRPC, and one agent with a stale token never blocks the other agents behind its egress IP. HTTP answers 429, gRPC `ResourceExhausted`.
- `middleware.StreamLimit` caps concurrent streams per client IP.

## Error model (shared)

A domain error maps to the same client-facing outcome on both transports because classification lives in **one** place: `errkind.Classify(err) → Kind`. 
Each transport only maps `Kind` to its wire form.

```text
                                    ┌─────────────────────────────┐
   domain sentinels ──────────────► │  errkind.Classify → Kind    │ ◄─── proxy AgentError
   (auth.*, storage.*, cluster.*,   │  (single source of truth)   │      (ErrorKind() tag)
    context.*)                      │                             │
                                    └──────────────┬──────────────┘
                                                   │
                            ┌──────────────────────┴───────────────────────┐
                            ▼                                              ▼
               http/response.FromError → status + helper        grpc/status.FromError → codes.*
```

Errors coming **from an agent** through the proxy are translated at the boundary:
`internal/proxy` is an anti-corruption layer that reads the agent's gRPC code / HTTP status and tags the error with a `Kind` (`AgentError.ErrorKind()`). 
Downstream code never sniffs gRPC/HTTP details - it just calls `errkind.Classify`, which trusts the tag. 
`NotFound` from an agent surfaces to the user as 404 / NotFound.

## Leader hint: `X-Leader`

`X-Leader` tells an HTTP client where the cluster leader is, and it has one meaning: the **HTTP address of the leader, `host:port` with no scheme**. It is the host of the leader's raft advertise address, as written, with the port of the listener the request came in on, which every replica is assumed to serve (the same-port assumption). It is a hint to retry at, not an order.

It goes out on:
- the 503 of a write that reached a follower (`cluster.ErrNotLeader`) or lost leadership while it was committing (`cluster.ErrLeadershipLost`), answered by `response.FromError`; the refresh of a session on a follower with `auth.rotate_refresh` is one;
- the 502 the `Leader` middleware answers when it cannot reach the leader it forwards a write to.

A 503 that carries it is a routing condition, not a failure of the node, so the access log (`Logger`) records it at WARN, where any other 5xx is an ERROR.

It is never sent when that address cannot be derived: this node is the leader, no leader is known, or the port of the listener is not known (`LeaderOptions.ForwardPort` is 0, which answers a write on a follower with a 503 without it). The leader's raft address is not sent in its place.

Every answer sets it through `response.SetLeader`, from the one derivation of the address (`leaderHTTPAddr` in the `Leader` middleware, which is also where a write is forwarded to). The middleware hands it to the handlers as a function in the request context (`httpctx.Leader`), asked when the answer is written, since the leader can change while a request runs. The gRPC interceptors' `x-leader` metadata is another hint: it carries the leader's raft address.

## TLS

`tlsconf` turns PEM file config into `*tls.Config`; the shared `config.TLS` block drives it. 
Both halves are opt-in (empty = plaintext):
- `tls.server` → the CP's serving identity for the HTTP, HTTP-discovery, and gRPC listeners (`client_ca_file` enables mTLS - client cert required).
- `tls.client` → the CP as a client when dialing agents (proxy); verifies agent certs and optionally presents a client cert.

Applied uniformly: one server `*tls.Config` for all listeners, one client config for the proxy pool and for the `Leader` middleware,
whose follower forwards writes to the leader over TLS when `tls.server` is set (`LeaderOptions.TLSClientConfig`).
The raft transport between nodes has TLS of its own, `cluster.raft.tls` (see `internal/raft`).
