# Package database

Package database elects podium's leader through the database, for the `database`
mode, whose replicas hold no state: a lease that one replica holds at a time, a
fence on every write transaction of the store, and a registry of the replicas.
`Leadership` is the mode's `cluster.Leadership`, `cluster.Membership` and
`sqlstore.Fence`: what the HTTP and gRPC forwarding, the trust of a forwarding
replica's `X-Forwarded-For` and the shared-settings check take, as they take
raft's, and the fence the store is built with. `New` builds it from `Options`;
`Run` takes part in the election until its context ends.

## Package map

```text
database/
├── leadership.go   Options, New, Leadership: AmLeader, CurrentLeader, WhenLeader,
│                   Run; the terms and the step-down; the package doc
├── lease.go        the lease's statements: take, renew, release, the followers'
│                   read; the statement timeout
├── fence.go        Check, the fence of the store's write transactions
└── registry.go     the replica registry: the heartbeat, Members, the pruning
```

## The lease

`podium_leader` holds one row: its `epoch`, its `holder` (a replica's ID), the
`address` the others forward writes to (the holder's advertise address), and
`expires_at`, in the database's clock, so that the replicas' clocks never
compare.

- A replica takes the row when it is free (no row, or a released one) or
  expired, in one statement that raises the epoch, also when this replica held
  it last: an `INSERT … ON CONFLICT DO UPDATE … WHERE expires_at <= now()`. A
  row that another replica holds, unexpired, is left as it is
- The holder renews the row every third of the lease, an `UPDATE` of
  `expires_at` that matches its epoch and its ID. A renewal that changes no row
  finds the lease moved, and steps the replica down at once
- The holder steps down, `AmLeader` false, the contexts of `WhenLeader`
  cancelled and its writes refused, once no renewal has succeeded within two
  thirds of the lease, counted from the send of the last renewal that did, not
  from its answer: it stops a third of the lease before the row can expire,
  however long that renewal took. A renewal that answers after the step-down
  revives nothing
- The lease's statements run on a pool of their own on the primary
  (`Options.LeaseDB`, one connection), so that writers never starve them. Each
  runs under a `statement_timeout` the server enforces even on a statement that
  waits for a lock: a sixth of the lease, cut for a renewal to the time left
  before the step-down, so that none lands after it, and never under 10 ms. A
  context deadline a statement timeout later covers a server that does not
  answer
- When `Run`'s context ends, the replica steps down, which cancels its
  `WhenLeader` work first, then releases the row: `expires_at` becomes the
  database's now, epoch and holder kept, so that its writes stay fenced until
  another replica takes over. A planned restart, such as a rolling upgrade, so
  moves the leadership within a second instead of a lease

## The fence

`Check` runs at the start of each write transaction of the store, before its
first write:

- a replica that does not hold the lease is refused, without a statement
- the holder reads the row `SELECT epoch, holder … FOR SHARE` and compares both
  with its own term's: the holder too, so that a database failover that brings
  an older row back cannot let two replicas pass with one epoch. A row that
  moved, a takeover this replica has not seen yet, or a missing one, is refused,
  and steps the replica down at once

Both refusals answer `cluster.ErrNotLeader`, with messages of their own
(`this replica does not hold the lease`, `the lease moved to another replica`):
the fence runs before the first write, so nothing was written, as raft answers
`ErrNotLeader` for a write whose term moved before it proposed it. The
transports answer it with 503, and HTTP adds the leader's address when one is
known.

The share lock holds the row until the transaction ends. A takeover, which locks
the row to update it, waits for the write in flight, and a fence that runs after
a takeover reads the row it left: an ex-leader that has not yet noticed cannot
commit. The leader's own renewals wait for its write in flight too: a write
transaction that holds the row past the step-down deadline steps the leader
down, and still commits, as no takeover lands before it ends. podium's write
transactions take milliseconds.

## Followers

A replica that does not hold the lease reads the row every second, on the
lease's pool, and answers `CurrentLeader` from what it read: the holder's
address while that lease lasts by the time the read gave, `""` once it ended,
for a released or expired row, and for this replica's own row after its
step-down. It takes the lease when it reads it free or expired, and looks again
at the expiry it read (and a hundredth of the lease), so that it takes an
expired lease at once rather than at its next read. The holder answers its own
advertise address.

## The registry

Each replica heartbeats its row of `podium_replicas` every second: its ID, its
advertise address, the digests of the settings every replica must share
(`cluster.SharedSettings.Digests`, the five groups in order, 160 bytes), and
`last_seen` in the database's clock. The heartbeat reads the replicas seen
within three heartbeats, which `Members` answers, each as a `cluster.Peer` of
its ID and advertise address, by ID. A call reads nothing: it filters the last
list by the time each replica was seen, so that a replica drops out three
heartbeats after its last whether or not the heartbeats that follow succeed. The
replica itself is among its members while its own heartbeats succeed.

When `Run`'s context ends, a replica deletes its row. The leader prunes, every
minute, the rows unseen for an hour, of replicas that stopped without deleting
theirs.

## Timings

| What | When |
| -- | -- |
| the lease (`Options.Lease`) | 10 s when zero; 60 ms at least |
| a renewal | every third of the lease, after the send of the last |
| the step-down | two thirds of the lease after the send of the last renewal that succeeded |
| a lease statement's timeout | a sixth of the lease, cut to the step-down for a renewal; 10 ms at least |
| a retry after a lease statement that failed | a twelfth of the lease; a follower, a second at most |
| a follower's read of the lease row | every second, and at the expiry it read |
| a heartbeat | every second |
| a member | seen within three heartbeats |
| the registry's pruning | by the leader, every minute, the rows unseen for an hour |

## The failover window

- A leader that crashes, or loses the database, renews nothing: the row expires
  a lease after its last renewal, and another replica takes it about a hundredth
  of the lease after the expiry, at a raised epoch. A leader that lost the
  database stepped down a third of the lease before. Writes answer 503 from the
  crash, or the step-down, until the takeover: at most a lease after the last
  renewal that landed, 10 s by default
- A leader that stops cleanly releases the row: another replica takes it at its
  next read, within a second
- A leader whose process pauses past its lease, and wakes once another replica
  took over: its step-down deadline has passed on its own clock, and a write the
  clock lets through finds the row moved, which the fence refuses, stepping the
  leader down

For a moment the old leader's `WhenLeader` work and the new one's may both run:
the fence refuses the old one's writes; its calls to agents are not fenced.

## WhenLeader

`WhenLeader` keeps raft's semantics: while the replica leads, it runs `fn` with
a context cancelled when the term ends, which the end of the term does before
anything else; after a term ends it waits for the next and runs `fn` again. An
`fn` that returns nil while the replica still leads runs again at once, without
raft's barrier, as the database holds every committed write: `fn` must return
only once its context ends. Only the caller's context, `Run`'s end, as raft's
shutdown, or an error of `fn` ends it.

## Logs and errors

Every line names the replica ID (`replica_id`) and its advertise address
(`advertise_addr`), and the epoch when one is in play; never a database URL, a
host of the database or a credential. The first failure of a run of lease
statements, or of heartbeats, logs a WARN, the success that ends the run an
INFO; the takeover, the release and the pruning an INFO, a step-down a WARN.

Errors are classified by `sqlstore.Postgres`, as the store's are: an error names
the SQLSTATE at most, never a row, a statement's argument, `PgError.Detail` or a
connection string. The statements are Postgres's; a second engine brings its
own.

## Tests

The tests take a database each from `pgtest` and skip while
`PODIUM_TEST_POSTGRES_URL` is unset, but for `TestNewValidatesOptions`;
`task ci/golang/test/postgres` runs them against a Postgres container. Their
leases are seconds long, and they wait on conditions:

- `TestOneLeader`: one of two replicas leads, both know where, and its term
  holds across renewals past the two thirds of the lease the takeover alone
  gives
- `TestFailoverWithinLease`: the leader's lease pool closed between two
  renewals, the other leads within the lease, at a raised epoch, never both at
  once
- `TestExLeaderWriteFenced`: an ex-leader that has not seen the takeover writes
  nothing, and steps down at once
- `TestTakeoverWaitsForAFencedWrite`: a takeover waits for the write in flight
- `TestSameHolderNewEpoch`: a replica that takes the lease again raises the
  epoch, once the row expired
- `TestStepDownBeforeExpiry`: a slow renewal lands, then renewals stop landing:
  the leader steps down two thirds of the lease after that renewal's send, not
  its answer, before the row expires
- `TestReleaseOnStop`: a stopped leader releases the lease and deletes its row,
  and the other leads well within the lease
- `TestMembersFromRegistry`, `TestRegistryPrunedByTheLeader`: the members, their
  digests, a crashed replica's drop-out, the pruning
- `TestFollowerWriteWithoutLeaderIs503`: a follower that knows no leader refuses
  a write, which HTTP answers 503
- `TestWhenLeaderRaftSemantics`: `fn` runs again at each acquisition, and while
  it returns early
