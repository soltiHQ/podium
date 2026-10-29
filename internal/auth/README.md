# internal/auth
Authentication, authorization, and session management subsystem.

## Package map
```text
auth/
├── error.go              sentinel errors (12 exported)
│
├── credentials/          password hashing and verification (bcrypt)
├── identity/             authenticated principal (Identity struct)
├── providers/            Provider interface + Request/Result contracts
│   └── password/         password provider implementation
├── ratelimit/            in-memory rate limiter for failed attempts
├── rbac/                 RBAC permission resolver (user + role merge)
├── session/              core service: login, refresh, revoke
├── token/                Issuer / Verifier / Clock abstractions
│   └── jwt/              HS256 implementation (issuer + verifier)
└── wire/                 composition root (NewAuth)
```

## Login flow
access.Service.Login → Limiter.Reserve(account, client IP); see "Login rate limiting"
```text
  handler
    │
    ▼
  wire.Auth.Session.Login(ctx, authKind, subject, secret)
    │
    ├─ 1. authenticate                  (providers/password.Provider)
    │      ├─ find user by subject
    │      ├─ find credential
    │      └─ bcrypt verify              (credentials.VerifyPassword)
    │
    ├─ 2. resolve permissions            (rbac.Resolver)
    │      └─ user direct ∪ role perms → sorted, de-duped
    │
    ├─ 3. create session                 (storage)
    │      ├─ generate session ID        (16-byte random hex)
    │      └─ store refresh hash         (SHA3-256)
    │
    ├─ 4. issue access token             (token/jwt.HSIssuer → HS256 JWT)
    │
    └─ 5. return TokenPair + Identity
```

## Refresh flow
```text
  Refresh(ctx, sessionID, refreshRaw)
    │
    ├─ load session from storage
    ├─ check: not revoked, not expired
    ├─ constant-time hash comparison     (subtle.ConstantTimeCompare)
    ├─ check: user not disabled
    ├─ RBAC permission resolution
    ├─ rotate refresh token (if enabled)
    └─ issue new access token
```

## Dependency graph
```text
  wire.NewAuth()                         ← composition root
    │
    ├── session.Service                  ← core orchestrator
    │     ├── providers.Provider         (interface)
    │     ├── token.Issuer               (interface)
    │     ├── token.Clock                (interface)
    │     ├── session.RBACResolver       (interface)
    │     └── storage.Storage            (interface)
    │
    ├── jwt.HSVerifier                   ← token validation
    │     ├── token.Clock
    │     └── identity.Identity
    │
    ├── rbac.Resolver                    ← permission merge
    │     └── storage.Storage
    │
    ├── password.Provider                ← credential verification
    │     ├── credentials.*
    │     └── storage.Storage
    │
    └── ratelimit.Limiter                ← brute-force protection
```

## Key types

| Package        | Type            | Purpose                                              |
|----------------|-----------------|------------------------------------------------------|
| `identity`     | `Identity`      | authenticated principal with permissions             |
| `session`      | `Service`       | login / refresh / revoke orchestration               |
| `session`      | `TokenPair`     | access + refresh token pair                          |
| `session`      | `Config`        | TTL, issuer, audience, rotation flag                 |
| `providers`    | `Provider`      | interface: `Kind()` + `Authenticate()`               |
| `token`        | `Issuer`        | interface: `Issue(ctx, identity) → string`           |
| `token`        | `Verifier`      | interface: `Verify(ctx, raw) → identity`             |
| `token`        | `Clock`         | interface: `Now()` — testability                     |
| `ratelimit`    | `Limiter`       | thread-safe in-memory attempt tracker                |
| `rbac`         | `Resolver`      | user + role permission union                         |
| `wire`         | `Auth`          | composition root (Clock, Limiter, Session, Verifier) |

## Security model

| Concern              | Implementation                                           |
|----------------------|----------------------------------------------------------|
| Password hashing     | bcrypt, cost 12 (clamped to `[bcrypt.MinCost, 31]`)      |
| Token signing        | HMAC-SHA256 (HS256)                                      |
| Refresh token        | 32-byte `crypto/rand`, base64 raw URL encoded            |
| Refresh validation   | SHA3-256 hash, `subtle.ConstantTimeCompare`              |
| Session ID           | 16-byte `crypto/rand`, hex encoded                       |
| Rate limiting        | per account + per client IP, bounded, in memory (below)  |
| Error masking        | generic errors hide which field failed                   |

## Login rate limiting
Before authenticating, `access.Service.Login` reserves one slot on two keys of `kit.Auth.Limiter` at once
(`Limiter.Reserve`): `login:acct:<subject>` (trimmed and lower-cased like user storage) and `login:ip:<client IP>`
(IPv6 counted by its /64, IPv4-mapped addresses unmapped).

- An attempt is refused with `ErrRateLimited` (HTTP 429) while, for either key, failures plus attempts in flight
  reach `auth.rate_attempts`; a refused attempt reserves nothing. A burst of parallel attempts therefore gets at
  most `auth.rate_attempts` password checks per key.
- A failed check turns the slot into a failure on both keys (`Attempt.Fail`). `auth.rate_attempts` failures, each
  within `auth.rate_window` of the previous one, block a key for `auth.rate_window`.
- The failure that blocks a key returns its usual error joined with `ErrRateLimitReached`;
  the UI answers it like any failed login and records one `rate_limited` event.
- A successful login releases the slots (`Attempt.Release`) and resets the account key only. Nothing resets a client IP key.
- The User-Agent plays no part. The limiter keeps at most 100 000 keys and evicts the least recently updated one;
  a key with an attempt in flight is neither evicted nor forgotten.
