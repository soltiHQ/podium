# Package sqlstore

Package sqlstore keeps the control-plane state in a SQL database, for the
`database` mode: podium's replicas hold no state, and the database holds it. It
runs on `database/sql`, with a `Dialect` for what differs between engines:
placeholders, the classification of the driver's errors and the schema's
migrations. Postgres is the first dialect, through pgx's `database/sql` driver
(`github.com/jackc/pgx/v5/stdlib`, registered as `pgx`).

So far the package is the store's core: `Open`, the write gate, the
transactions, the read routing, the error classification, the cursor, the
migrator and the Postgres schema. The entities' methods, which make `Store` a
`storage.Storage`, are not written yet; they build on this core and keep the
rules below.

## Package map

```text
sqlstore/
├── store.go        Store, Options, Open (no migration), Fence, AllowAll; the package doc
├── dialect.go      Dialect: Name, Rebind, Classify, Migrations
├── postgres.go     Postgres: $n placeholders, the SQLSTATE classification, the embedded migrations
├── migrate.go      Migrate, SchemaVersion, the migration files
├── migrations/postgres/0001_init.sql   the Postgres schema
├── tx.go           the write transactions: the gate, the fence, nesting
├── readonly.go     the read routing: the replica for marked listings, the primary otherwise
├── cursor.go       the listing cursor, tagged "sql"
├── errors.go       ErrSchemaNewer, ErrSchemaOlder, what every dialect's classification shares
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
- no column checks more than the store does in Go: a lookup's column is indexed,
  not unique, as in the in-memory store, and a lookup that finds two rows
  answers `storage.ErrInternal`
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
  `payload`, with the `created_at` they are pruned by

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
- a statement that can still fail runs inside a savepoint, rolled back to on
  failure

## Reads

Point lookups, and every read of a transaction, read the primary. Listings,
scans and counts read the replica (`Options.Replica`), when there is one, for a
context marked `storage.MarkReadOnly` outside a transaction, and the primary
otherwise: runners, auth and transactions decide on what they read, and a
replica may lag. `Open` checks the primary's schema alone.

The cursor of a listing is a place in the listing order: the creation time, in
nanoseconds, and the ID's bytes of the row it was made at, as base64url JSON
tagged `sql`. A cursor that is not the store's, the in-memory store's included,
or a malformed one, is `storage.ErrInvalidArgument`.

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
| a failed connection attempt (`*pgconn.ConnectError`, a timed-out one included), SQLSTATE class 08, 57P01 to 57P03, 57P05 (an idle-session timeout), `driver.ErrBadConn`, `pgconn.ErrConnClosed`, a network error, a stream cut short | `storage.ErrUnavailable` |
| `context.Canceled`, `context.DeadlineExceeded`, and any error once the caller's context is done, 57014 included | the context's error itself |
| anything else, SQLSTATE 57P04 (the database was dropped) included | `storage.ErrInternal` |

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
`go test -race` through the Go container on that network with
`PODIUM_TEST_POSTGRES_URL` set, and removes the container and the network on
every exit. Its packages are the Taskfile's `postgres_test_packages`, which each
package of the database mode joins. CI runs it with the other Go checks.

The server is the current stable image pinned by the digest of its multi-arch
index, so that it runs on arm64 and amd64 hosts alike:
`postgres:18.7@sha256:9f4fd4aa918f52ae3d6f3b687f1de90d8f867cd4d520021545a9a50c120eae3b`
(pinned 2026-11-13; `postgres_image` in the Taskfile). To move it, pin the index
digest of the new tag, as `docker buildx imagetools inspect postgres:<tag>`
prints it.
