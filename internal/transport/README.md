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
    Negotiate    ── picks the Responder (JSON vs HTML), stores it in ctx
     CrossOrigin ── refuses cross-origin browser mutations: 403 through that Responder.
                    It runs before any route, so a refusal gets no request id and no
                    access-log line; CrossOrigin logs it at WARN instead. It also runs
                    before Leader, so a follower refuses such a request itself: it never
                    reaches the leader.
      Leader     ── raft only: forwards writes to the leader, over TLS with tls.client when tls.server is set,
                    straight to it, never through HTTP(S)_PROXY, marked X-Podium-Forwarded by its transport,
                    after the reverse proxy dropped the hop-by-hop headers, and never forwards a marked write
                    again; drops the leader's CORS headers from its answer, as CORS above has set the follower's;
                    stores where the leader serves this listener in ctx (httpctx.Leader), for X-Leader
       mux
        │  per route:
  RequestID      ── installs request id + error slot (API routes and the event stream only)
   Logger        ── reads the slot back after the handler returns (same routes); logs 5xx at ERROR, but a 503 with X-Leader at WARN
    Auth / OptionalAuth / RequirePermission  ── 503 when credentials cannot be checked, logged at ERROR; a refresh that needs the
                                                 raft leader (auth.rotate_refresh on a follower) is 503, with X-Leader when the leader is known, logged at WARN
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

`Negotiate` and `CrossOrigin` run ahead of `Leader`, and nothing depended on the old order: `Negotiate` only stores a
`Responder`, which the leader picks again for a request a follower forwards, `Leader` answers its own 502 and 503 in plain
text without it, and `CORS` stays outermost, as before. The reverse proxy adds the leader's headers to the ones the follower
set, so `Leader` drops the leader's CORS headers, every `Access-Control-*` header and the `Origin` token of `Vary`, and
keeps the rest: an answer that names the origin twice is one a browser refuses (`TestForwardedWriteCarriesTheCORSHeadersOnce`).

### gRPC

```text
  gRPC client
        ▼
  UnaryRecovery → UnaryRequestID → UnaryLogger → UnaryLeader
        │   (UnaryAuth / UnaryRequirePermission exist but are not chained yet)
        │   UnaryLeader, raft only: a follower forwards a Sync to the leader (Forwarder, never through
        │   HTTPS_PROXY) and answers with the leader's answer; any other write is Unavailable + x-leader;
        │   a call it passes on carries the means to hint the leader, for a handler's leadership error (HintLeader)
        │   UnaryLogger logs a failed call at ERROR, but a "not the leader" answer with x-leader at WARN
        │   and a throttle (ResourceExhausted with RetryInfo, status.Throttled) at DEBUG
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

`clientip.Resolver` decides who the client of a request is (`trusted_proxies`, see `internal/config`):
- `IP(r)` - the TCP peer; when the peer is a trusted proxy, the right-most `X-Forwarded-For` entry that is not itself trusted.
- `Forwarded(peer, values)` - the same walk over any forwarding values, such as the gRPC `x-forwarded-for` metadata.
- `HTTPS(r)` - direct TLS, or `X-Forwarded-Proto: https` from a trusted proxy. With `auth.cookie_secure: auto` it decides
  whether the auth and device cookies are `Secure`; `app` builds that decision once and hands it to every cookie writer.

In raft mode the trusted proxies are `trusted_proxies` and the hosts of the current raft configuration (`clientip.Hosts`),
since a follower forwards writes to the leader: the leader believes the forwarding headers of another node and of the
proxies `trusted_proxies` lists, and of nobody else. See "Forwarding and trusted hosts" in `internal/raft/README.md` for the
rule, names included.

RFC 7239 `Forwarded` is not supported. The login limiter, `middleware.StreamLimit` and HTTP discovery take the client IP from the resolver.
gRPC discovery does too in raft mode, from the peer and its `x-forwarded-for` metadata, which a follower sets when it
forwards a sync; standalone gRPC discovery uses the peer address, whatever metadata the call carries.

## Rate limits

- The main listener has no limit on failed authentication: its tokens and session IDs cannot be guessed, and passwords go through the login limiter (`internal/auth`).
- Discovery has no per-IP limit. Both Sync handlers reserve an attempt per (client IP, agent ID) before the token check, in one shared limiter, so checks in flight and failures count
  together and across HTTP and gRPC, and one agent with a stale token never blocks the other agents behind its egress IP. HTTP answers 429, gRPC `ResourceExhausted`.
- Discovery also limits how fast each agent ID syncs: a bucket of 10 syncs, one back per second, shared by HTTP and gRPC, looked at before the token check (a sync that finds it empty is refused without touching the store) and charged after it.
  HTTP answers 429 with `Retry-After: 1` (`response.TooManyRequests`), gRPC `ResourceExhausted` with `RetryInfo` (`status.Throttled`). See `internal/config`, "Rate limiting".
  Such an answer is a throttle, which any client can cause at will, so `Logger` and `UnaryLogger` record it at DEBUG, where they record any other 4xx at WARN
  and any other failed call at ERROR; a follower that proxies or forwards the leader's answer logs it at DEBUG as well, the header and the detail being the leader's.
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
- the 503 of the removal of the leader through the membership API, which hands the leadership over first and answers with `response.NotLeader`, given the leader's address the handler read once, so that the removal is repeated at the new leader (see "Cluster" in `internal/handler/README.md`);
- the 503 of a discovery sync that met a leadership error on the node that served it (`response.StoreFailed`);
- the 502 the `Leader` middleware answers when it cannot reach the leader it forwards a write to;
- the 503 the `Leader` middleware of a follower answers for a write another node forwarded already: a forward carries
  `X-Podium-Forwarded`, and a follower never forwards a write that carries it again, so a write crosses one node at most,
  even when two followers take each other for the leader, through a stale view or ports that differ between nodes
  (`TestLeaderForwardsAWriteOnce`). A client that sends the header itself only keeps its own write from being forwarded;
  the leader serves a marked write as any other. gRPC marks a forwarded call the same way, see below.

A 503 that carries it is a routing condition, not a failure of the node, so the access log (`Logger`) records it at WARN, where any other 5xx is an ERROR.

It is never sent when that address cannot be derived: this node is the leader, no leader is known, or the port of the listener is not known (`LeaderOptions.ForwardPort` is 0, which answers a write on a follower with a 503 without it). The leader's raft address is not sent in its place.

Every answer sets it through `response.SetLeader`, from the one derivation of the address (`cluster.LeaderAddr`, which the `Leader` middleware also forwards a write to). The middleware hands it to the handlers as a function in the request context (`httpctx.Leader`), asked when the answer is written, since the leader can change while a request runs. A handler that chooses its answer by the address asks once, and answers with what it read: two asks can give two leaders, or one and none.

### gRPC: `x-leader`

The gRPC `x-leader` header metadata has the same meaning for the gRPC listener: the **gRPC address of the leader, `host:port`**, the host of the leader's raft advertise address, as written, with this node's gRPC port (`grpc.addr`), from the same derivation, so the same-port assumption holds for gRPC too. It goes out with the `Unavailable` of a write the follower does not forward: a write stream, a unary write the `Forwarder` has no call for, a call another node forwarded already (`x-podium-forwarded`, so a call crosses one node at most; as with `X-Podium-Forwarded`, a client that sends it itself only keeps its own call from being forwarded), and a forward the leader never answered (`leader unreachable`). A `Sync` that a follower forwards gets the leader's own answer instead, and its hint when that answer is the `Unavailable` of a node that does not lead, which refused the call as a forwarded one. As for HTTP, it is not sent when no leader is known or the gRPC port is unknown, and the raft address is never sent in its place.

A `not the leader` answer with the hint is a routing condition, so `UnaryLogger` logs it at WARN, where any other failure is an ERROR: an `Unavailable` without the hint, and `leader unreachable`, which keeps its hint but is a failure, as HTTP logs the 502 of an unreachable leader at ERROR (`TestUnaryLoggerLogsRoutingAnswersAtWarn`). A call that `UnaryLeader` passed on carries in its context how to ask where the leader is: a handler whose call meets a leadership error all the same, as gRPC discovery does on a leader that lost its leadership, answers it with `interceptor.HintLeader`, which sets the hint of the leader the node knows then, and the mark of a routing answer, so `UnaryLogger` logs it at WARN as well; with no leader known it sets neither. `StreamLogger` logs every stream at INFO, a failed one included, as before.

## TLS

`tlsconf` turns PEM file config into `*tls.Config`; the shared `config.TLS` block drives it. 
Both halves are opt-in (empty = plaintext):
- `tls.server` → the CP's serving identity for the HTTP, HTTP-discovery, and gRPC listeners (`client_ca_file` enables mTLS - client cert required).
- `tls.client` → the CP as a client when dialing agents (proxy); verifies agent certs and optionally presents a client cert.

Applied uniformly: one server `*tls.Config` for all listeners, one client config for the proxy pool, for the `Leader` middleware,
whose follower forwards writes to the leader over TLS when `tls.server` is set (`LeaderOptions.TLSClientConfig`), and for the
gRPC `Forwarder`, which forwards a follower's syncs under the same rule: TLS with `tls.client` when `tls.server` is set, checked
for the leader's raft host, with `tls.client`'s certificate presented when the leader asks for one. Both forwards dial the
leader directly: the `Leader` middleware's transport has no proxy, and the `Forwarder` dials with `grpc.WithNoProxy`, so neither
takes `HTTP_PROXY` or `HTTPS_PROXY` from the environment.
The raft transport between nodes has TLS of its own, `cluster.raft.tls` (see `internal/raft`).
