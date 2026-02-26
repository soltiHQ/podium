# internal/service
Application-level use-cases built on top of `storage` contracts.
Each sub-package owns a single domain aggregate and exposes a `Service` struct with business methods.

## Package map
```text
service/
├── helper.go         shared utilities (NormalizeListLimit)
│
├── access/           authentication: login, logout, permission listing
├── agent/            agent CRUD, label patching, heartbeat preservation
├── credential/       credential lifecycle, password creation, verifier cascade
├── session/          session retrieval, revocation, bulk deletion
├── spec/             spec CRUD, deployment (rollout fan-out), rollout queries
└── user/             user CRUD, cascading deletion, role validation
```

## Service anatomy
Every sub-package follows the same structure:

```text
<pkg>/
├── service.go   Service struct + New constructor + business methods
└── types.go     request/response DTOs (ListQuery, Page, …)
```

- `New(store, …)` panics on nil dependencies — fail-fast at startup.
- Methods accept `context.Context` as first argument.
- Returned entities are always **clones** — callers cannot mutate storage state.
- Errors are `storage.Err*` sentinels, compatible with `errors.Is()`.

## Dependency direction
```text
  handler / runner
        │
        ▼
    service/*          ← business rules, validation, orchestration
        │
        ▼
    storage.Storage    ← interface only, no concrete backend
```
Services depend on `storage.Storage` (interface), never on `inmemory` or any concrete backend.
Filters are created by the caller (handler) and passed through the service to the store.

## Shared helpers
| Function             | Purpose                                                                  |
|----------------------|--------------------------------------------------------------------------|
| `NormalizeListLimit` | Clamps page size: applies default if ≤ 0, caps at `storage.MaxListLimit` |
