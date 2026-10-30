# internal/storage
Persistence contracts for control-plane domain entities.  
Backend-agnostic interfaces: domain and application layers depend only on these, never on concrete implementations.

## Package map
```text
storage/
├── storage.go      store interfaces + aggregate Storage + optional Scanner
├── scan.go         All* helpers: every row of a listing, in one pass when the store is a Scanner
├── error.go        sentinel errors (ErrNotFound, ErrConflict …)
├── pagination.go   ListResult[T], ListOptions, limits
├── filter.go       backend-agnostic filter markers (AgentFilter, RolloutFilter …)
│
└── inmemory/
    ├── storage.go   Store — aggregates GenericStore instances, implements Storage
    ├── generic.go   GenericStore[T] — thread-safe CRUD for any domain.Entity[T]
    ├── scan.go      Scanner for Store and for its transaction handle
    ├── filter.go    concrete filters with builder API (ByLabel, ByStatus, Query …)
    └── cursor.go    opaque base64 cursor encoding / decoding
```

## Store interfaces
```text
  Storage (aggregate)
  ├── AgentStore        Upsert / Get / List / Delete
  ├── UserStore         Upsert / Get / GetBySubject / List / Delete
  ├── CredentialStore   Upsert / Get / GetByUserAndAuth / ListByUser / Delete
  ├── VerifierStore     Upsert / Get / GetByCredential / Delete / DeleteByCredential
  ├── SessionStore      Create / Get / ListByUser / RotateRefresh / Revoke / Delete / DeleteByUser
  ├── RoleStore         Upsert / Get / GetMany / GetByName / List / Delete
  ├── SpecStore         Upsert / Get / List / Delete
  └── RolloutStore      Upsert / Get / List / Delete / DeleteBySpec
```
Every method documents sentinel errors it may return.

## Error model
```text
  Error               When                                       Retryable?
  ─────               ────                                       ──────────
  ErrNotFound         entity does not exist                      no
  ErrAlreadyExists    create conflict (duplicate ID)             no
  ErrConflict         concurrent modification / version mismatch no
  ErrInvalidArgument  bad input, malformed cursor, wrong filter  no
  ErrUnavailable      temporary backend failure                  yes
  ErrInternal         unexpected / invariant-violating failure   no
```
All errors are compatible with `errors.Is()`.

## Pagination
```text
  caller                          storage
  ──────                          ───────
  ListAgents(filter, ListOptions{   ──→  snapshot + sort by (CreatedAt DESC, ID ASC)
      Limit:  50,                        slice [cursor … cursor+limit]
      Cursor: "…",                       encode last item as NextCursor
  })                               ◀──  ListResult{ Items, NextCursor }
        │
        ▼
  next page: ListOptions{ Cursor: result.NextCursor }
```

- **Ordering**: `(CreatedAt DESC, ID ASC)` — deterministic, no gaps or duplicates
- **Cursor**: opaque base64 JSON token, backend-validated
- **Limits**: 1 .. 500 (default 100)

## Scanning
`AllAgents`, `AllRollouts`, `AllSpecs` and `AllExecutions` return every row a filter matches.
Draining a listing makes an in-process backend scan and sort its whole store again for
every page of 500, so a backend that holds its rows in process implements the optional `Scanner`
interface, and the helpers use it:
```text
  AllAgents(ctx, store, filter)
      store is a Scanner  ──→  ScanAgents     one pass: filter, clone the matches, sort once
      any other store     ──→  CollectPages   ListAgents, page by page
```

- A scan returns what `CollectPages` returns: the same rows, in the listing order, as clones the caller owns
- The filter runs once per row, not once per row per page
- The rows are one consistent state, not one state per page
- `inmemory.Store` and its transaction handle, `durable.Store` and `raft.Store` are Scanners; the
  view that `raft.Store.WithTx` hands out is not, and is paged. A wrapper that embeds
  `storage.Storage` hides the interface and gets the paged path too: forward the four `Scan*`
  methods to keep the single pass
- A page asked for with `Limit` and `Cursor`, as the API and the UI do, is unchanged

## Filter pattern
Filters are **backend-specific**. The storage package declares empty marker interfaces (`AgentFilter`, `RolloutFilter` …).  
Each backend provides concrete types with builder methods:
```text
  inmemory.NewAgentFilter().
      ByPlatform("linux").
      ByLabel("env", "prod").
      Query("web-")                ──→  implements storage.AgentFilter

  inmemory.NewRolloutFilter().
      BySpecID("spec-42").
      ByStatus(kind.SyncStatusPending)  ──→  implements storage.RolloutFilter
```
Passing a filter from a wrong backend returns `ErrInvalidArgument`.

## In-memory implementation

### GenericStore[T]
Thread-safe CRUD for any `domain.Entity[T]`:
```text
  GenericStore[T domain.Entity[T]]
  ├── mu   sync.RWMutex
  └── data map[string]T

  Create(entity)           insert, fail if exists
  Upsert(entity)           insert or replace
  Update(id, fn(T) T)      load clone → apply fn → store (under lock)
  Get(id)        → clone   retrieve by ID
  GetMany(ids)   → clones  batch get, preserve order
  List(pred, opts) → page  match → sort → cursor → clone the page
  All(pred, less) → clones every match in one pass, sorted once (nil less = listing order)
  Delete(id)               remove by ID
```

- Entities are **cloned** on writing and on read — no shared mutable state
- Long scans check `ctx.Done()` every 1000 iterations
- `List` sorts the stored values and clones only the page it returns, all under one read lock
