# internal/auth
Authentication, authorization, and session management subsystem.

## Package map
```text
auth/
├── config.go             Config: JWT secret, TTLs, rate limits, cookie_secure
├── error.go              sentinel errors
│
├── credentials/          password hashing, verification and policy (bcrypt)
├── httpcookie/           the three auth cookies; the caller decides Secure
├── identity/             authenticated principal (Identity struct)
├── kit/                  composition root (kit.New)
├── providers/            Provider interface + Request/Result contracts
│   └── password/         password provider implementation
├── ratelimit/            in-memory rate limiter for failed attempts
├── rbac/                 RBAC permission resolver (user + role merge)
├── session/              core service: login, refresh, revoke
└── token/                Issuer / Verifier / Clock abstractions
    └── jwt/              HS256 implementation (issuer + verifier)
```

## Login flow
access.Service.Login → model.NormalizeSubject → Limiter.Reserve(account, client IP); see "Login rate limiting"
```text
  handler
    │
    ▼
  kit.Auth.Session.Login(ctx, authKind, subject, secret)
    │
    ├─ 1. authenticate                  (providers/password.Provider)
    │      ├─ find user by subject
    │      ├─ find credential
    │      └─ bcrypt verify              (credentials.VerifyPassword)
    │         unknown or disabled user, no password, or a password over
    │         72 bytes: bcrypt compare against a dummy hash, then
    │         ErrInvalidCredentials, as slow as a wrong password
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
  kit.New()                              ← composition root
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
| `kit`          | `Auth`          | composition root (Clock, Limiter, Session, Verifier) |

## Security model

| Concern              | Implementation                                                         |
|----------------------|------------------------------------------------------------------------|
| Password hashing     | bcrypt, cost 12 (clamped to `[bcrypt.MinCost, 31]`)                    |
| Password policy      | 12 to 72 characters when set (below)                                   |
| Token signing        | HMAC-SHA256 (HS256)                                                    |
| Access token         | `exp` required; `exp` and `nbf` allow 30 s of clock skew               |
| Refresh token        | 32-byte `crypto/rand`, base64 raw URL encoded                          |
| Refresh validation   | SHA3-256 hash, `subtle.ConstantTimeCompare`                            |
| Session ID           | 16-byte `crypto/rand`, hex encoded; not in the access log              |
| Auth cookies         | HttpOnly, SameSite Lax (refresh: Strict), Secure per `cookie_secure`   |
| Cross-site requests  | main listener refuses cross-origin browser mutations (403)             |
| Rate limiting        | per account + per client IP, bounded, in memory (below)                |
| Error masking        | generic errors hide which field failed                                 |
| Login timing         | unknown and disabled subjects pay one bcrypt compare (dummy hash)      |

## Passwords
`credentials.CheckPasswordPolicy` accepts a new password of `MinPasswordLength` (12) characters or more, counted as
runes, and of at most `MaxPasswordBytes` (72) bytes, the most bcrypt reads. Every way of setting a password applies it:
`credential.Service.SetPassword` (bootstrap) and `SetPasswordAs` (`POST /api/v1/users/{id}/password`, which answers
400 `Password must be 12-72 characters.`). Login does not apply it, so a shorter password set earlier still works; a
login password over 72 bytes is a wrong password, since bcrypt would compare only its first 72 bytes.

## Login rate limiting
Before authenticating, `access.Service.Login` normalizes the subject once (`model.NormalizeSubject`, trimmed and
lower-cased like user storage), then reserves one slot on two keys of `kit.Auth.Limiter` at once (`Limiter.Reserve`):
`login:acct:<subject>` and `login:ip:<client IP>` (IPv6 counted by its /64, IPv4-mapped addresses unmapped).

- An attempt is refused with `ErrRateLimited` (HTTP 429) while, for either key, failures plus attempts in flight
  reach `auth.rate_attempts`; a refused attempt reserves nothing. A burst of parallel attempts therefore gets at
  most `auth.rate_attempts` password checks per key.
- A wrong subject or password (`ErrInvalidCredentials`) turns the slot into a failure on both keys (`Attempt.Fail`).
  `auth.rate_attempts` failures, each within `auth.rate_window` of the previous one, block a key for `auth.rate_window`.
  Any other error (storage outage, token issue, user without permissions) releases the slots and counts nothing.
- The failure that blocks a key returns its usual error joined with `ErrRateLimitReached`;
  the UI answers it like any failed login and records one `rate_limited` event.
- A successful login releases the slots (`Attempt.Release`) and resets the account key only. Nothing resets a client IP key.
- The User-Agent plays no part. The limiter keeps at most 100 000 keys and evicts the least recently updated one;
  a key with an attempt in flight is neither evicted nor forgotten.
