# Package database

Package database elects podium's leader through the database, for the `database`
mode, whose replicas hold no state: a lease that one replica holds at a time, a
fence on every write transaction of the store, and a registry of the replicas.
`Leadership` is the mode's `cluster.Leadership`, `cluster.Membership` and
`sqlstore.Fence`: what the HTTP and gRPC forwarding, the trust of a forwarding
replica's `X-Forwarded-For` and the shared-settings check take, as they take
raft's, and the fence the store is built with. `New` builds it from `Options`;
`Run` takes part in the election until its context ends. `New` refuses a missing
pool, the primary's pool as the lease's (writers would starve the lease), an
empty replica ID or one a text column cannot hold, a negative lease or one under
`cluster.MinDatabaseLease` (60 ms), and an advertise address that
`cluster.CheckAdvertiseAddr` refuses: the configuration checks
`cluster.database.advertise_addr` and the lease with the same rules, which live
in `internal/cluster`, as this package imports it. `internal/app` wires it
(`buildDatabase`): see "Database mode" in `internal/app/README.md`.

The package also shares the event hub's events between the replicas:
`EventTransport` is the mode's `event.Writer`, through the table
`podium_events`, and `NewPostgresWaker` the `LISTEN` that wakes it (see
"Events").

## Package map

```text
database/
├── leadership.go   Options, New, Leadership: AmLeader, CurrentLeader, WhenLeader,
│                   Run; the terms and the step-down; the package doc
├── lease.go        the lease's statements: take, renew, release, the followers'
│                   read; the statement timeout
├── fence.go        Check, the fence of the store's write transactions
├── registry.go     the replica registry: the heartbeat, Members, the pruning
└── events.go       EventTransport, the event.Writer through podium_events: the
                    queue, the writer, the reader, the pruning; Waker and
                    NewPostgresWaker, the LISTEN that wakes it
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
  `WhenLeader` work first, waits a sixth of the lease at most for that work to
  return, then releases the row: `expires_at` becomes the database's now, epoch
  and holder kept, so that its writes stay fenced until another replica takes
  over. A planned restart, such as a rolling upgrade, so moves the leadership
  within a second instead of a lease. The app stops its leader-only runners, and
  waits for them up to its drain deadline (`server.shutdown_timeout`), before it
  ends `Run`'s context, so that the work has normally returned. Work that has
  not returned within that sixth of the lease, as runners that outlived the
  drain deadline, may still run: the row is not released then, and expires a
  lease after its last renewal, so that no other replica's leader-only work
  starts while it may (a WARN, `the leader-only work has not ended`;
  `TestNoReleaseWhileLeaderWorkRuns`). The registry row is deleted either way
- `Options.Observer` is told of the lease's events, for the metrics: `Renewed`
  after each renewal that landed, `Lost` each time a term ends before `Run`'s
  end, at the step-down deadline or once a renewal or a write found the lease
  moved; the release at `Run`'s end is no loss. The app counts them in
  `podium_database_lease_renewals_total` and
  `podium_database_lease_losses_total`, and reads `AmLeader` for
  `podium_database_leader`

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
advertise address. `Leader` answers the same holder with its replica ID, and
`WatchLeader` wakes a watcher, without blocking, at each change of it: when a
follower reads another holder, or none, and when this replica takes the lease or
steps down.

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

`Digests` reads a replica's digests from its row, by its ID: the shared-settings
check of the app (`raftpkg.NewConfigCheck`, the runner `config-check`) reads the
leader's, the replica `Leader` names, at each change of leader that
`WatchLeader` reports, and compares them with this replica's own, with raft's
handling of a mismatch (see "Configuration drift" in `internal/raft/README.md`).
A replica without a row is an error, which the check retries.

## Events

`EventTransport` is the mode's `event.Writer`: it shares the event hub's events
between the replicas through `podium_events`, so that the UI on every replica
sees what any replica sent. `NewEventTransport` builds it on the primary's pool,
with the dialect, a waker, the hub and the leadership; the app attaches it with
`hub.SetWriter` and runs it until its context ends (`Run`).

- `Notify` and `Record` queue the event. The transport's writer writes it as a
  row, and every replica applies it, through `ApplyLocalNotify` and
  `ApplyLocalRecord`, once it reads the row back: the writer's replica too,
  never at the call, so that each replica applies each event once, in the rows'
  order
- `DeleteIssues` removes the matching issues on its replica at the call and
  returns how many, as the handlers decide on that count, as on raft, then
  queues the deletion as a row, which every replica applies when it reads it.
  The writer's replica applies it again, in sequence, where it removes nothing
  more, unless an issue whose row came before the deletion's reached it after
  the call: that one goes, as on every other replica
- A row is a kind of podium's own (`notify`, `record`, `delete_issues`), never
  the event's, which `text` would refuse with a NUL, and a payload, the
  protojson of raft's op for the event (`event_notify`, `event_record`,
  `event_delete_issues`), in `bytea`. Its sequence comes from the table's
  identity, its `created_at` from the database's clock
- The queue holds 1024 events, and queueing never waits. An event that finds it
  full, or the transport stopped, or that does not encode (a string that is not
  UTF-8, which raft's encoding refuses too), never reaches the database: it is
  applied on its replica alone, as raft's `EventWriter` falls back, and counted
  (`Dropped`)
- The writer takes the queued events, 128 at most, and writes them in one
  transaction, one attempt within 5 s: the dialect's event lock (`EventLockSQL`,
  a transaction-level advisory lock for Postgres), a row per event, and the
  dialect's wake-up (`NotifySQL`, `NOTIFY "podium_events"`), which goes out with
  the commit. The lock makes the rows' sequence order their commit order, so
  that a reader that reads the rows after the last sequence it read never skips
  a row that committed after a later one; readers take no lock. A write that
  fails before `COMMIT` applies its events on its replica alone and counts them;
  one whose `COMMIT` fails may have written them, and leaves them to the reads.
  Nothing is retried
- Each replica reads the rows after the last sequence it read, in sequence: when
  its waker wakes it, after each write of its own, and on an interval, every 5 s
  with a waker, so that a listener that died without a word delays events that
  long, and every second without one. It starts after the newest row present
  when `Run` starts: the hub's history stays transient, as in the other modes,
  and the events queued before then are written once `Run` knows where it starts
- A row of an unknown kind, or whose payload does not decode as an event of its
  kind, is skipped and logged without its payload, and the reads go on past it:
  a newer release never stalls an older one during a rolling upgrade. The fields
  of a payload this release does not know are dropped
- The leader prunes, at the start of its term and every minute, the rows older
  than ten minutes by the database's clock, under `WhenLeader`. A replica that
  falls further behind than that misses what was pruned
- When `Run`'s context ends, the events still queued, and every event sent
  after, are applied on their replica alone and counted

The waker of Postgres, `NewPostgresWaker`, listens on `EventsChannel`
(`podium_events`) through a connection of its own, never one of the store's
pool, with `application_name` `podium-events`. Its `Wait` returns on a
notification, and on each connection, the first and every reconnect, as rows may
have come while it did not listen. A connection that fails or is lost is closed,
and the next `Wait` connects again after a pause: 100 ms after the first
failure, doubling up to 5 s. Its errors pass through `sqlstore.Postgres`'s
classification: a refused connection names its SQLSTATE and nothing else, and a
connection string that does not parse is an error of its own that never quotes
it.

`LISTEN` needs a direct connection to the primary, or a pooler in session mode:
PgBouncer in transaction mode delivers no notification, and every replica then
sees the others' events only on its reads every 5 s. A notification sent while a
listener reconnects is lost, and the reconnect's own read covers it.

## Timings

| What | When |
| -- | -- |
| the lease (`Options.Lease`) | 10 s when zero; 60 ms at least |
| a renewal | every third of the lease, after the send of the last |
| the step-down | two thirds of the lease after the send of the last renewal that succeeded |
| `Run`'s end, waiting for the `WhenLeader` work before the release | a sixth of the lease at most |
| a lease statement's timeout | a sixth of the lease, cut to the step-down for a renewal; 10 ms at least |
| a retry after a lease statement that failed | a twelfth of the lease; a follower, a second at most |
| a follower's read of the lease row | every second, and at the expiry it read |
| a heartbeat | every second |
| a member | seen within three heartbeats |
| the registry's pruning | by the leader, every minute, the rows unseen for an hour |
| the event queue | 1024 events |
| a write of events | 128 at most in one transaction, one attempt within 5 s |
| a read of events, a prune | within 5 s each |
| a read of events without a wake-up | every 5 s with a waker, every second without |
| a reconnect of the waker | 100 ms after the first failure, doubling up to 5 s; its connection and `LISTEN` within 5 s |
| the events' pruning | by the leader, every minute, the rows older than ten minutes |
| a log line of one kind of the transport's failures | once a minute at most |

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
(`advertise_addr`), and the epoch when one is in play; never a database URL or a
credential. The package adds no host of the database itself: the app's logger
carries the primary's host, port and database (`host`, `port`, `database`),
which podium logs in place of a URL. The first failure of a run of lease
statements, or of heartbeats, logs a WARN, the success that ends the run an
INFO; the takeover, the release and the pruning an INFO, a step-down a WARN.

Errors are classified by `sqlstore.Postgres`, as the store's are: an error names
the SQLSTATE at most, never a row, a statement's argument, `PgError.Detail` or a
connection string. The statements are Postgres's; a second engine brings its
own, and its dialect the events' lock and wake-up, or no wake-up, and the
transport then polls.

The events transport logs a failure, of a write, a read, the waker, a prune, an
event that never reached the database or a row it skipped, at WARN, once a
minute at most per kind, with `held_back`, how many lines of the kind it held
back since the last; a prune that deleted rows logs at DEBUG. No line carries an
event's payload, a statement's argument or a connection string.

## Tests

The tests take a database each from `pgtest` and skip while
`PODIUM_TEST_POSTGRES_URL` is unset, but for `TestNewValidatesOptions`, which
checks `New`'s refusals, the primary's pool as the lease's among them;
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
- `TestNoReleaseWhileLeaderWorkRuns`: a leader stopped while its `WhenLeader`
  work does not return leaves the lease to expire, and the other leads once it
  expired
- `TestMembersFromRegistry`, `TestRegistryPrunedByTheLeader`: the members, their
  digests, a crashed replica's drop-out, the pruning
- `TestDigestsFromRegistry`: `Digests` reads each replica's digests, and refuses
  a replica without a row
- `TestLeaseObserved`: the renewals counted, a step-down and a moved lease
  counted as losses, a clean stop not
- `TestLeaderWatched`: `Leader` and the wake-ups of `WatchLeader` as a follower
  reads the holder, and as it takes the lease once the holder stopped
- `TestFollowerWriteWithoutLeaderIs503`: a follower that knows no leader refuses
  a write, which HTTP answers 503
- `TestWhenLeaderRaftSemantics`: `fn` runs again at each acquisition, and while
  it returns early

The events transport's tests turn the reads on an interval off, or shorten them,
so that only the path under test can deliver an event:

- `TestEventsReachEveryReplica`: pulses and records from several goroutines on
  two transports at once reach both hubs through the wake-ups, each once, in the
  rows' order, each goroutine's in the order sent
- `TestEventsResumeAfterReconnect`: the events written while a listener's
  session is ended are applied once it reconnects, which alone wakes it
- `TestEventQueueBounded`: with the writes stalled by a lock, no call waits, the
  events past the queue are applied on their replica alone, counted and logged,
  and the queue is written in order once the lock goes
- `TestEventWritesSerialized`: a write waits for the event lock another
  transaction holds, and takes a sequence after that one's row
- `TestDeleteIssuesCountsLocally`: the count at the call, the deletion applied
  by the other replica, and again by its writer, in sequence
- `TestRunStartsAfterTheNewestRow`, `TestUnreadableRowsSkipped`: where the reads
  start, and the rows a replica cannot read
- `TestEventsPruned`: a follower's transport keeps the old rows, and prunes them
  by the database's clock once its replica leads
- `TestWakerErrorsNeverCarryURL`: a wrong password, a host that does not
  resolve, a closed port and a URL that does not parse fail the waker without
  its URL, user, host or password
