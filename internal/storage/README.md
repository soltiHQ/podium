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
├── validate.go     ValidateEntity — the row check the in-memory store and the overlay share
│
├── inmemory/
│   ├── storage.go   Store — aggregates GenericStore instances, implements Storage
│   ├── generic.go   GenericStore[T] — thread-safe CRUD for any domain.Entity[T]
│   ├── scan.go      Scanner for Store and for its transaction handle
│   ├── filter.go    concrete filters with builder API (ByLabel, ByStatus, Query …)
│   └── cursor.go    opaque base64 cursor encoding / decoding
│
└── overlay/
    ├── overlay.go   Tx — a transaction over an unchanging base: reads its own writes, records them as ops
    ├── apply.go     ApplyOps — applies recorded ops to a store, as the Raft state machine does
    ├── command.go   EncodeCommand / DecodeCommand — the SOLTI-CMD framing of the ops
    └── agent.go …   the Storage methods of Tx, one file per entity group
```

## Store interfaces
```text
  Storage (aggregate)
  ├── AgentStore        Upsert / Get / List / Delete
  ├── AgentCredentialStore  Upsert / Get / List / Delete
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
  ListAgents(filter, ListOptions{   ──→  sort the stored values by (CreatedAt DESC, ID ASC), under the read lock
      Limit:  50,                        clone the page [cursor … cursor+limit]
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
- `inmemory.Store` and its transaction handle, `durable.Store`, `raft.Store` and the `overlay.Tx`
  that `raft.Store.WithTx` and `durable.Store.WithTx` hand out are Scanners. A wrapper that embeds
  `storage.Storage` hides the interface and gets the paged path too: forward the four `Scan*`
  methods to keep the single pass
- A page asked for with `Limit` and `Cursor`, as the API and the UI do, is unchanged

## Transaction overlay
`overlay.Tx` holds a transaction in memory over a base store that does not change while it runs.
`raft.Store.WithTx` runs its callback on one and replicates what it recorded; `durable.Store.WithTx`
logs it to disk, then applies it to the state its readers see:
```text
  tx := overlay.New(base)      writes land in tx; every read of tx sees them over the base
  tx.UpsertAgent(ctx, a)       checked as the state machine will apply it, recorded as a SOLTI-CMD op
  tx.Ops(), tx.Empty()         the recorded ops in write order; nothing to commit when empty
  overlay.EncodeCommand(ops)   the SOLTI-CMD command Raft replicates and the durable store logs
  overlay.ApplyOps(ctx, s, …)  applies the ops to s, as the Raft state machine does on commit
```

- Point reads, index lookups, listings and scans merge the base with the transaction's writes. A
  page of a listing is cut from one scan of the base merged with those writes, so each page clones
  every match where the in-memory store clones the page alone; users and roles, which have no
  scan, drain the base's listing instead. Nothing rebuilds a store
- A write the state machine would refuse fails in the transaction and records nothing. Deleting a
  row the transaction does not see gives `ErrNotFound`, as the stores do
- A bulk delete removes the rows it matches when it is called, the transaction's own included,
  and not the rows written after it. One that matches none records nothing, so `Empty` is exact

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
