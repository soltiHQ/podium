# Package sqlstore

Package sqlstore keeps the control-plane state in a SQL database, for the
`database` mode: podium's replicas hold no state, and the database holds it. It
runs on `database/sql`, with a `Dialect` for what differs between engines:
placeholders, the classification of the driver's errors, the schema's
migrations, and the notification and the lock of the events transport
(`internal/cluster/database`). Postgres is the first dialect, through pgx's
`database/sql` driver (`github.com/jackc/pgx/v5/stdlib`, registered as `pgx`).

The package is the store's core — `Open`, the write gate, the transactions, the
read routing, the error classification, the cursor, the migrator and the
Postgres schema — and the methods of the entities on it, which make `Store` a
`storage.Storage` and keep the rules below. `storagecontract.Run` checks it, as
it checks the in-memory store and raft.

## Package map

```text
sqlstore/
├── store.go        Store, Options, Open (no migration), Fence, AllowAll; the package doc
├── dialect.go      Dialect: Name, Rebind, Classify, NotifySQL, EventLockSQL, Migrations
├── postgres.go     Postgres: $n placeholders, the SQLSTATE classification, NOTIFY and the
│                   events' advisory lock, the embedded migrations
├── migrate.go      Migrate, SchemaVersion, the migration files
├── drop.go         DropSchema, and the tables of the schema it drops
├── migrations/postgres/0001_init.sql   the Postgres schema
├── tx.go           the write transactions: the gate, the fence, nesting, WithTx
├── readonly.go     the read routing: the replica for marked listings, the primary otherwise
│                   and when the replica is unavailable
├── cursor.go       the listing cursor, tagged "sql"
├── errors.go       ErrSchemaNewer, ErrSchemaOlder, what every dialect's classification shares
├── keys.go         the cap on a value bound to an indexed column, bytea binding
├── rows.go         the reader and the writer the entities' methods are on; lookups, deletes, upserts
├── list.go         the keyset listing, with the Go filter applied to its batches
├── filter.go       the filters of the factory: the criteria SQL applies, the Query Go applies
├── scan.go         the Scan methods, in one snapshot
├── count.go        the Count methods, by GROUP BY
├── agents.go, agent_credentials.go, specs.go, rollouts.go, executions.go,
│   users.go, credentials.go, verifiers.go, sessions.go, roles.go
│                   one entity group each: its codec, its columns, its methods
├── contract_test.go   storagecontract.Run on a Postgres database
├── bench_test.go   BenchmarkAgentSyncs, against the in-memory store (see Performance)
└── pgtest/         a fresh Postgres database per test, for the tests of the database mode
```

## Schema

`0001_init.sql` creates one table per entity group — `podium_agents`,
`podium_agent_credentials`, `podium_users`, `podium_credentials`,
`podium_verifiers`, `podium_sessions`, `podium_roles`, `podium_specs`,
`podium_rollouts`, `podium_executions` — and `podium_leader` (the database
leadership's lease), `podium_replicas` (the replica registry), `podium_events`
(the events every replica applies) and `podium_schema_migrations` (the
migrations applied).

An entity's row is its ID, its creation time, the columns its lookups, criteria
and counts read, and its body:

- the body is the protojson of the entity's snapshot message, the message raft
  already versions (`snapshot.MarshalAgentJSON` and the like), and carries every
  field; the other columns only index it. A field a newer podium wrote, which
  this one does not know, is ignored when the body is read
- every column that holds an entity's string — an ID, a foreign ID, a subject, a
  name, an auth kind — is `bytea`, and so is the body. `bytea` compares and
  orders by bytes, as Go compares strings, with no collation, and carries a NUL
  (`\u0000`), which a model string may hold and `text` and `jsonb` refuse.
  Statements bind such a column as `[]byte(s)` and scan it into `[]byte`: a Go
  string bound to a `bytea` parameter is read as `bytea`'s text input form, not
  as its bytes
- `created_at`, and any other time a statement compares, is a `bigint` of Unix
  nanoseconds, 0 for the zero time. That holds the years 1678 to 2262 only: the
  store refuses a time outside them with `storage.ErrInvalidArgument`, never
  wraps it
- the body holds every time of the entity as Unix nanoseconds too, so the entity
  codecs refuse, with `storage.ErrInvalidArgument`, any time of the entity
  outside the years 1678 to 2262, indexed or not, and the exact Unix epoch where
  the snapshot message reads 0 back as unset: every entity but the session,
  whose message says which of its times are set. Raft, whose log and snapshots
  carry the same messages, does not refuse them today: it wraps a time outside
  those years silently, and reads the epoch back as the zero time (a creation
  time at the epoch it then refuses, as missing). The in-memory store keeps them
  as they are
- a status podium fills from its own enum is a `smallint`, or `text` (the
  execution record's)
- no constraint checks more than the store does in Go, but for the size of an
  index entry: a lookup's column is indexed, not unique, as in the in-memory
  store, and a lookup that finds two rows answers `storage.ErrInternal`. A btree
  entry is capped near 2.7 kB, and an `INSERT` of a larger key fails with
  SQLSTATE 54000, which aborts the transaction it runs in. So the store refuses,
  in Go and before any statement, a value it binds to an indexed column — an ID,
  a foreign ID, a subject, a role name — longer than 2048 bytes (the values of a
  composite index, a credential's user ID and auth kind, together), with
  `storage.ErrInvalidArgument`, and the transaction stays usable. One helper
  does it for every entity, and only for the values a write binds: a lookup of a
  longer value finds nothing, as in memory. This is a divergence: memory and
  raft accept such keys. podium's own IDs are short and an agent ID is at most
  128 bytes, so only a REST client's username or role name over 2 KiB meets it
- the body is read into the model: an older podium that rewrites a row a newer
  one wrote drops the fields it does not know. That is acceptable for the
  rolling upgrades the design allows, whose migrations only add, and raft's
  state machine loses such fields the same way
- each table has an index in the listing order, `(created_at DESC, id ASC)`

The other tables are the database mode's own, and the leadership and the events
transport that use them may change them:

- `podium_leader` is the lease, one row: its `epoch`, raised by each takeover,
  its `holder` (a replica's ID), the `address` the others forward writes to, and
  `expires_at` in the database's clock
- `podium_replicas` is the registry: a row per replica, its ID, its
  `advertise_addr`, the `digests` of the settings every replica must share, and
  `last_seen` in the database's clock
- `podium_events` holds the events in sequence (`seq`), each a `kind` and a
  `payload`, with the `created_at` they are pruned by: the events transport of
  `internal/cluster/database` writes and reads them, under the dialect's event
  lock (`EventLockSQL`), with the dialect's wake-up (`NotifySQL`)

## Writes

- Every write runs in a write transaction: that of `WithTx`, or one of its own
  for a write method called outside `WithTx`
- One write transaction runs at a time in a process, behind the store's gate.
  Waiting for the gate honours the context
- A write transaction runs on the primary at READ COMMITTED
- The store's `Fence` checks each write transaction before anything else runs in
  it, before `WithTx`'s callback, as raft refuses `WithTx` on a follower before
  the callback. A refusal comes back unchanged, and nothing is written. Without
  a fence the store allows every write (`AllowAll`), for tests and a store that
  is its database's only writer
- The callback runs once: nothing is retried. Its error, or a context done
  before the commit, rolls the transaction back; a context done before the
  transaction runs no callback
- A context that ends while `COMMIT` is in flight can leave the transaction
  committed while the caller gets the context's error: the caller cannot tell.
  That is inherent, and raft's apply timeout behaves alike
- A transaction inside the callback is the same one, without a savepoint: a
  nested callback's error rolls nothing back by itself, and the fence does not
  check it again

### Refused writes keep the transaction usable

Postgres aborts a transaction at its first failed statement: every statement
after it fails until the transaction rolls back. A write the store refuses
inside a transaction must leave the transaction usable, as the in-memory store
does: the user service goes on after a delete answers `ErrNotFound`, and
commits. So the store refuses in Go, before any statement:

- the entity's validation (`storage.ValidateEntity`, the codecs, the execution
  record's rules) runs before the statement
- `ErrNotFound` comes from the rows a statement affected, never from a failed
  statement
- an insert-if-absent and `ErrAlreadyExists` go through `ON CONFLICT`, never
  through a unique violation
- the checks run in Go before the write, the inserts use `ON CONFLICT`, and
  `ErrNotFound` comes from the rows affected, so no statement is expected to
  fail inside a transaction; there is no savepoint

## Reads

Point lookups, the by-user listings (`ListCredentialsByUser`,
`ListSessionsByUser`) and every read of a transaction read the primary.
Listings, scans and counts read the replica (`Options.Replica`), when there is
one, for a context marked `storage.MarkReadOnly` outside a transaction, and the
primary otherwise: runners, auth and transactions decide on what they read, and
a replica may lag. The HTTP layer marks the GET and HEAD requests of the UI and
API listener (`middleware.ReadOnly`). `Open` checks the primary's schema alone.

A marked read that fails on the replica with `storage.ErrUnavailable` (the
replica down or unreachable, a connection it refused or ended, a statement it
cancelled, as a hot standby does for a conflict with its recovery) runs once
more, on the primary, while the context is live: a replica that is down never
fails a listing the primary can answer. Any other error of the replica, and the
error of a context that ended, comes back as it is. Each marked listing tries
the replica first, so a replica that drops packets rather than refuse costs each
listing its connection attempt, which the URL's `connect_timeout` bounds.

The cursor of a listing is a place in the listing order: the creation time, in
nanoseconds, and the ID's bytes of the row it was made at, as base64url JSON
tagged `sql`. A cursor that is not the store's, the in-memory store's included,
or a malformed one, is `storage.ErrInvalidArgument`.

## Entities

Every entity group is a file: its table and codec, the columns its row holds
beside the body, its writes on the `writer` and its reads on the `reader`. The
store embeds the reader, so a point lookup runs on the primary's pool; its
writes, outside `WithTx`, run through the core's write transaction (gate, fence,
a transaction of their own); the handle `WithTx` gives is a writer on that
transaction, and so are its reads.

- **Writes validate in Go first.** `storage.ValidateEntity` (no nil, no ID, no
  creation time), then the entity's codec, which refuses a string that is not
  UTF-8 and a time outside the years 1678 to 2262, then the cap on a key: all
  before a statement, so a refused write leaves the transaction usable. An
  upsert is `INSERT … ON CONFLICT (id) DO UPDATE`. A delete answers
  `ErrNotFound` from the rows it deleted. A bulk delete (`DeleteRolloutsBySpec`,
  `DeleteSessionsByUser`, `DeleteVerifierByCredential`) deletes every row the
  key holds and is idempotent; `DeleteVerifierByCredential` deletes every
  verifier of the credential, as memory does
- **Listings** run in the order `(created_at DESC, id ASC)` by keyset: the rows
  after the place the cursor names. SQL applies the exact criteria and the
  order; nothing else. A page is one statement of `limit+1` rows when no Go
  filter applies. The filtered listings below read keyset batches until they
  hold `limit+1` matches or the rows end, so a page is full unless the rows are
  over, no match is skipped or returned twice across pages, and the cursor names
  the last row returned. A page with no row is an empty list
- **What SQL filters, what Go filters.** SQL applies the exact criteria: an
  agent's `StaleAtBefore` (a deadline before the bound, or none: `stale_at` 0
  means never set, which the codec's refusal of the exact Unix epoch keeps
  unambiguous); a rollout's `SpecID`, `AgentID` and `Statuses`; an execution
  record's `AgentID`, `SpecID`, `TaskName`, `TaskUID` and `Statuses`. Go applies
  `Query`, over the rows SQL returns, as `storage.TextQuery` does for the
  in-memory store (the query trimmed and folded with `strings.ToLower`, a field
  matches when its folded value contains it): agents over ID, name and endpoint;
  users over subject, name and email; specs over name and slot. `%`, `_` and `\`
  are literal, and case folds beyond ASCII, as in memory, whatever the engine's
  collation. The bodies of the rows are decoded for it: a search that matches
  fewer rows than a page reads and decodes every row, which "Performance"
  measures for the agents
- **A criterion time outside the range of the column** is not refused: every
  stored time lies inside it, so the answer is the same for every row, and the
  filter builds it so. An agent's `StaleAtBefore` below the year 1678 matches
  the agents with no deadline, and above 2262 every agent, as memory answers
- **An execution record's `Statuses`** that are not terminal match nothing, as
  no stored record has one, and are left out of the statement
- **A filter** the factory did not build, one built for another entity, or a nil
  typed pointer, is `ErrInvalidArgument`; a nil filter lists every row
- **Lookups** (`GetUserBySubject`, `GetCredentialByUserAndAuth`,
  `GetVerifierByCredential`, `GetRoleByName`) compare exactly, by bytes: no
  trimming, no case folding. The columns are indexed, not unique, as in memory:
  two rows answer `ErrInternal`, naming the table and no value. An upsert that
  changes a subject, a role name or a link moves the row: the lookup of the old
  value finds nothing
- **By-user listings** (`ListCredentialsByUser`, `ListSessionsByUser`) return
  every row of the user in the listing order, never a page, nil for none, from
  the primary like the lookups: auth decides on them
- **Roles.** `GetRoles` answers one role per ID asked for, in the order asked,
  repeats included (each a copy of its own), and `ErrNotFound` when one is
  missing. The store builds no role filter, so `ListRoles` takes only a nil
  filter: any other is `ErrInvalidArgument`, as for every filter this store did
  not build. The factory builds no criteria on a user's roles or permissions
- **Sessions.** `CreateSession` of an existing ID answers `ErrAlreadyExists`
  from `ON CONFLICT DO NOTHING` and the rows it inserted, with no failed
  statement. `RotateRefresh`, `RotateRefreshAt` and `RevokeSession` are
  read-modify-writes of the body through the model's methods, in one write
  transaction, on the row read `FOR UPDATE`: a zero time keeps the stored
  modification time, the same hash and expiry change nothing, the first
  revocation wins. `RotateRefresh` stamps the time of the call. A rotation or a
  revocation whose result the codec cannot carry (a time after 2262) is
  `ErrInvalidArgument`, before any write. No error of a session method names its
  ID: it is the session cookie
- **Executions.** `UpsertExecution` validates the record as the model does
  (`model.NewExecutionRecord` of its parameters, its ID against its run's), then
  inserts only a new ID: `ON CONFLICT (id) DO NOTHING`. A replay is no error and
  changes nothing, its capture time included
- **Scans.** A `Scan*` outside a transaction is one read-only REPEATABLE READ
  transaction on the database the routing picks; inside a transaction it reads
  that transaction. It returns the rows in the listing order, nil when none
  matches
- **Counts.** `CountAgents` and `CountRollouts` are one `GROUP BY` statement: an
  empty map, not nil, when no row exists, and no entry for a status no row has.
  Inside a transaction they count the transaction
- **Context.** Every method honours its context: a done one answers its own
  error, before a statement for a listing, a scan and a count

## Errors

The dialect classifies the driver's errors, and the store returns the error of a
done context as itself. For Postgres:

| Error | Store answers |
| -- | -- |
| SQLSTATE 23505, a unique violation | `storage.ErrAlreadyExists` |
| SQLSTATE 40001 and 40P01, a serialization failure and a deadlock | `storage.ErrUnavailable` |
| SQLSTATE 25006, a write that reached a read-only server, as a demoted primary | `storage.ErrUnavailable` |
| SQLSTATE 57014, a statement timeout or a cancel on the server's side, while the caller's context is live | `storage.ErrUnavailable` |
| SQLSTATE 55P03, a lock timeout | `storage.ErrUnavailable` |
| SQLSTATE class 53, a lack of resources (disk, memory, connections) | `storage.ErrUnavailable` |
| a failed connection attempt (`*pgconn.ConnectError`, a timed-out one included; the SQLSTATE of the server's refusal, as 28P01 or 3D000, when it carries one), SQLSTATE class 08, 57P01 to 57P03, 57P05 (an idle-session timeout), `driver.ErrBadConn`, `pgconn.ErrConnClosed`, a network error, a stream cut short | `storage.ErrUnavailable` |
| `context.Canceled`, `context.DeadlineExceeded`, and any error once the caller's context is done, 57014 included | the context's error itself |
| anything else, SQLSTATE 57P04 (the database was dropped) included | `storage.ErrInternal` |

An error that already wraps a storage sentinel is returned as it is, unless a
`*pgconn.PgError` is in its chain: that one is classified by its SQLSTATE. A
`*pgconn.ConnectError` wrapping the caller's own deadline answers
`storage.ErrUnavailable` from the dialect, which cannot see the caller's
context; the store checks its context first and returns the context's error, and
a caller outside the store does the same.

An error the store returns names the SQLSTATE and the table, column and
constraint at most. It never carries `PgError.Detail` or `Message`, which quote
rows and arguments, a statement's arguments, a row's data, or a connection
string, whose user, host and password a connection error would name. No log line
carries a statement's arguments.

## Migrations

`Migrate` brings a database's schema to the latest of the dialect's migrations,
`NNNN_name.sql` files embedded per dialect, versions 1, 2, 3… without a gap. It
applies each missing file in a transaction of its own, under a lock on
`podium_schema_migrations` (`LOCK TABLE … IN EXCLUSIVE MODE`), and records it
there, so replicas that start together migrate once. The migrator creates
`podium_schema_migrations` before the first migration, as it must lock it; two
sessions that create one table at once can fail in Postgres on a unique index of
the catalog (`pg_type`), which the migrator takes for the table being there. A
database already at the latest version gets no statement but reads.

`Open` migrates nothing and refuses a schema other than the latest:
`ErrSchemaNewer` for a newer one, which a later podium wrote (a rollback, or two
versions during a rollout), and `ErrSchemaOlder` for an older one, an empty
database included ("run podium migrate"); both name the two versions. `Migrate`
on a newer schema answers `ErrSchemaNewer` and writes nothing. `SchemaVersion`
reads the version, 0 for a database without `podium_schema_migrations`.

A release's migrations only add: tables, columns and indexes, never a change
that the version before cannot read, so that the version before keeps working
while a rollout runs, and a database whose schema is newer than a replica's
stops that replica (`ErrSchemaNewer`). The role that migrates needs to create
tables in its schema; `internal/config/README.md`, "Operating the database
backend", has the operator's side: privileges, connections, replicas that lag
and the password.

`DropSchema` drops the schema: every table the migrations create, listed in
`drop.go` next to them (a migration that adds a table adds it there, which
`TestMigrateCreatesSchema` checks), the rows with them, and nothing else, no
other table, no schema of the database's, not the database. It runs in one
transaction at READ COMMITTED: with none of the tables present it ends, dropping
nothing; otherwise it locks `podium_schema_migrations`
`IN ACCESS EXCLUSIVE MODE`, which waits for a migrator in flight and holds off
the next one and the read of `SchemaVersion` with which a replica opens the
store; reads the version under the lock and refuses a newer schema with
`ErrSchemaNewer`, as it can hold tables this podium does not know; asks its
guard, in the transaction, under the lock, whose refusal comes back as it is
(`database.CheckStopped` for `podium drop-schema`, which refuses while a replica
runs); then drops the tables in one statement. It returns the tables it dropped.
A database it dropped is an empty one to `Open` and to `Migrate`.

Until the database mode is released, `0001_init.sql` stays the only migration
and is edited in place: no installation exists. The lock statement and the
catalog lookup of `SchemaVersion` (`to_regclass`) are Postgres's; a second
engine brings its own with its dialect.

## Tests and the Postgres gate

The tests that need Postgres take a database from `pgtest`: `NewDatabase`
creates an empty database on the server `PODIUM_TEST_POSTGRES_URL` names
(`postgres://user:password@host:port/database`, a user that may create
databases) and drops it when the test ends, `Open` opens a pool on one, `Fresh`
does both. While the variable is unset those tests skip, so
`task ci/golang/test` runs without Postgres.

`task ci/golang/test/postgres` runs them with the race detector against a
Postgres container: it removes what a killed run left behind, creates a docker
network, starts the server, waits until it accepts connections over TCP, runs
`go test` twice through the Go container on that network with
`PODIUM_TEST_POSTGRES_URL` set, and removes the container and the network on
every exit. The first invocation tests the Taskfile's `postgres_test_packages`,
which each package of the database mode joins, with the flags
`postgres_test_flags` (`-race -count=1 -timeout=10m`); the second runs the tests
of `internal/app` and `cmd` that need Postgres (`postgres_app_test_packages`,
the names `postgres_app_test_run` matches) with `postgres_app_test_flags`, the
same. Each of these variables can be set on the command line, as "Performance"
does. CI runs the task with the other Go checks.

The server is the current stable image pinned by the digest of its multi-arch
index, so that it runs on arm64 and amd64 hosts alike:
`postgres:18.7@sha256:9f4fd4aa918f52ae3d6f3b687f1de90d8f867cd4d520021545a9a50c120eae3b`
(pinned 2026-11-13; `postgres_image` in the Taskfile). To move it, pin the index
digest of the new tag, as `docker buildx imagetools inspect postgres:<tag>`
prints it.

## Performance

`bench_test.go` holds `BenchmarkAgentSyncs`, in a test package of its own
(`sqlstore_test`), as it fences the store with the database leadership, which
imports the store. It runs on two backends: `postgres`, the SQL store on a fresh
database of its own, on a pool of the database mode's default 10 connections,
its write transactions fenced by a `database.Leadership` that holds the lease,
as in the database mode; and `memory`, the in-memory store of the memory mode.
Each is seeded, in one transaction, with the P2a fixture of
`internal/storage/storagetest` at 100, 1000 and 5000 agents: 20 specs, and per
agent a stored credential and 3 rollouts. The benchmark then times the sync of
an enrolled agent as discovery runs it, one after another, as a replica's store
runs one write transaction at a time: verify the token, a write transaction that
writes nothing while the token matches, read the agent, and upsert it, a write
transaction. It reports the syncs a second (`syncs/s`) and the median and the
99th percentile of one (`p50-ms`, `p99-ms`, from `storagetest.Measure`). Then it
times 20 listings as the UI's agent search runs them (`agent.Service.List` of
the factory's criteria, the default page of 30, on a context marked read-only),
each a search for the ID of the agent in the middle of the fleet, which matches
that agent alone, so that the SQL store reads and decodes every row, in batches
of 64 (see "Entities"), and reports their median (`list-p50-ms`). The `postgres`
sub-benchmarks skip without `PODIUM_TEST_POSTGRES_URL`, so
`task ci/golang/bench` runs the `memory` ones once, as a smoke check. The
numbers are indicative, not a gate.

### Conditions

The Task containers on an Apple M3 Max Mac (14 cores, 36 GiB) in OrbStack
(Docker 29.8.2, linux/arm64, 14 vCPUs), Go 1.27.4, without `-race`, against the
server of `task ci/golang/test/postgres`: `postgres:18.7` at the digest above,
on the gate's docker network, its data on tmpfs, with `fsync`,
`synchronous_commit` and `full_page_writes` off. Measured between 18:30 and
18:32 UTC on 2026-11-14, on `555c5f9` with the change that added the benchmark,
by the gate with its first invocation's flags and packages set, and its second
invocation running nothing:

```sh
task ci/golang/test/postgres postgres_test_flags='-count=3 -timeout=30m -run=^$ -bench=^BenchmarkAgentSyncs$ -benchtime=1000x' postgres_test_packages=./internal/storage/sqlstore/ postgres_app_test_run='^$'
```

A cell is the median of the three runs.

| backend | agents | syncs a second | sync p50 | sync p99 | search p50 |
| -- | -- | -- | -- | -- | -- |
| postgres | 100 | 3454 | 0.18 ms | 3.2 ms | 1.9 ms |
| postgres | 1000 | 3473 | 0.18 ms | 3.3 ms | 23.6 ms |
| postgres | 5000 | 3379 | 0.18 ms | 3.4 ms | 135.8 ms |
| memory | 100 | 12023 | 0.034 ms | 1.3 ms | 0.008 ms |
| memory | 1000 | 2088 | 0.26 ms | 4.5 ms | 0.087 ms |
| memory | 5000 | 561 | 1.4 ms | 4.6 ms | 0.48 ms |

### Reading them

- A sync on Postgres costs the same at every fleet size: lookups and upserts by
  key. The in-memory store's write transaction first copies every map of the
  store, to roll back to (`takeSnapshot`), so its sync grows with the state: two
  copies a sync.
- 5000 agents that sync every 30 s, the fixture's heartbeat, need 167 syncs a
  second. The server here flushes nothing to disk, unlike raft's commits in
  `internal/raft/README.md`. Each of a sync's two write transactions takes the
  fence's key-share lock on the lease row, which writes WAL, so a server that
  flushes its WAL at each commit (`synchronous_commit` on, the default) adds
  that flush twice to a sync, and a synchronous standby its acknowledgement as
  well. Neither was measured.
- The search at 5000 agents reads and decodes every agent's body: 136 ms here,
  in proportion to the rows (about 25 µs a row), where the in-memory store takes
  0.5 ms.
