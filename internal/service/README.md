# internal/service
Application-level use-cases built on top of `storage` contracts.
Each sub-package owns a single domain aggregate and exposes a `Service` struct with business methods.

## Package map
```text
service/
├── helper.go         shared utilities (NormalizeListLimit)
├── error.go          ErrNilService, which the handler constructors panic with on a nil service
│
├── access/           authentication: login, logout, permission listing
├── agent/            agent CRUD, label patching, heartbeat preservation, credential reset, deletion with rollouts
├── cluster/          raft membership: status, adding and removing members (over cluster.Admin, not storage)
├── credential/       credential lifecycle, password creation, verifier cascade
├── history/          run history: the archive of observed terminal runs (capture, listing, retention)
├── role/             role CRUD
├── session/          session retrieval, revocation, bulk deletion
├── spec/             spec CRUD, deployment (rollout fan-out), rollout queries
└── user/             user CRUD, cascading deletion, role validation, permission ceilings, last admin
```

## Service anatomy
Every sub-package follows the same structure:

```text
<pkg>/
├── service.go   Service struct + New constructor + business methods
└── types.go     request/response DTOs (ListQuery, Page, …)
```

- `New(store, …)` panics on nil dependencies — fail-fast at startup; `history.New` returns an error instead.
- Methods accept `context.Context` as first argument.
- Returned entities are always **clones** — callers cannot mutate storage state.
- Errors are `storage.Err*` sentinels, a sub-package's own (such as `user.ErrLastAdmin` or `agent.ErrUnauthenticated`),
  or the errors of `internal/auth`, all compatible with `errors.Is()`.

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
The one exception is `cluster`, which changes no state of the store: it depends on `cluster.Admin` (interface), the
administration of a raft node, never on hashicorp/raft, and exists in raft mode only.

## Shared helpers
| Function             | Purpose                                                                  |
|----------------------|--------------------------------------------------------------------------|
| `NormalizeListLimit` | Clamps page size: applies default if ≤ 0, caps at `storage.MaxListLimit` |
