# Durable standalone storage

Standalone uses durable storage by default in `data/podium`, relative to the
process working directory. For a stable absolute deployment location, configure:

```yaml
cluster:
  backend: standalone
  standalone:
    data_dir: /var/lib/solti/podium
```

Equivalent environment:
`SOLTI_CLUSTER_STANDALONE_DATA_DIR=/var/lib/solti/podium`. Use a
deployment-owned local directory and keep it across process/container restarts.
Mount a persistent volume for a container deployment. Do not share this
directory between standalone replicas or switch a Raft data directory to it. The
startup log explicitly identifies persistent or ephemeral standalone storage.
For disposable development only, set `cluster.standalone.ephemeral: true` or
`SOLTI_CLUSTER_STANDALONE_EPHEMERAL=true`. This explicit flag takes precedence
and ignores DataDir, including the default. A blank DataDir without that flag
fails configuration validation instead of silently discarding restart state.

`podium.db` contains all storage entities, including credentials, sessions, task
specifications, rollout intent/state and agent credentials: the raw token of a
stored credential, of an agent that enrolled without the enrollment secret or
that an older version enrolled, and no token for an agent enrolled under the
secret (see "Enrollment secret" in `internal/config/README.md`). New directories
use mode 0700; the database uses 0600. Existing directories and database files
with group/world permissions or symlinks, and files whose state does not check
out, are rejected without resetting state. No existing permissions are changed;
the one change to existing data is the migration of a single-blob file, below.
The existing event hub remains transient; this does not persist event/SSE
history.

## Upgrading

The first start of this version on a `podium.db` of the single-blob layout,
which versions before format 2 wrote, migrates it, once, to format 2 when its
snapshot is version 3, and earlier versions refuse the migrated file. A
single-blob file whose snapshot is version 2 is refused, as this version reads
version 2 no more: start the version before this one on it first, which migrates
it to format 2 with a version-3 snapshot, and then this one. A file in format 2
opens as it is: the versions that wrote it wrote version 3. So stop Podium and
copy `podium.db` first: a cold backup. Then start the new version, which
migrates a single-blob file and logs one warning. Rolling back needs that copy.

This version writes its snapshot and its log entries in version 4, which carries
derived agent credentials, and the version before it reads version 3 at most: it
refuses a file this version wrote to with its version error
(`unsupported command version 4`, `unsupported version 4`), and every earlier
version refuses the file too. That holds from the first write after the upgrade,
as every snapshot and log entry this version writes is version 4. Rolling back
needs the cold backup above. The raw tokens already in the file stay there until
the store compacts them: the snapshot and the log entries written before the
upgrade until the next snapshot replaces them, and the freed pages of the file
until bbolt writes over them. Take a fresh backup after that, and drop the older
ones, which hold the raw tokens too.

## Log and snapshots

The file holds three bbolt buckets, format 2:

- `meta`: `format` (2) and `snapshot_index`, the last log entry the snapshot
  holds;
- `snapshot`: `blob`, the state at `snapshot_index` in the version-4 snapshot
  codec Raft uses, and its `sha256`;
- `log`: the entries after `snapshot_index`, by index. An entry is the
  `SOLTI-CMD` version-4 command of one transaction, the encoding Raft logs,
  followed by the CRC-32C of its index and the command.

Both are read in version 3 as well, which the version before this one wrote
(`TestV3StoreLoads`), so a file it left opens with the state it holds, and the
entries this version appends follow its version-3 ones.

Numbers and keys are 8-byte big-endian integers.

A transaction runs its callback on an in-memory overlay of the published state,
which records each write as a command operation and checks it as the apply will.
A transaction that recorded nothing returns without touching the disk. Otherwise
one bbolt transaction appends the command at the next index, and bbolt syncs it
before the commit returns: the data pages, then the meta page. Only then are the
operations applied to the published state, with the same `ApplyOps` the Raft
state machine runs, under a lock that readers share and that the apply holds
only while it applies, so readers see all of a transaction or none of it and
never wait for a sync. Transactions are serialized: one holds the writer across
its callback, its commit and its apply, so the log order is the apply order.
Queued transactions honor their context deadline, and reads do not queue behind
a transaction. Criteria-only filters do not wait for anything.

Callback errors and cancellation observed before the commit change nothing. Once
bbolt starts the commit, a successful commit wins concurrent cancellation and
its operations are applied. A commit error publishes nothing and makes the store
unavailable until restart: a failed sync may leave the disk outcome uncertain,
so continuing from memory would be unsafe. A write whose commit returned an
error may still be on disk; if it is, it is present after the restart. A
committed command whose apply fails, or does not return because it panics, also
makes the store unavailable: memory then no longer matches the disk, and the
failure names the log entry. After any of these, reads and writes return
`storage.ErrUnavailable` with the reason, and the store passes the reason once,
as it fails, to the handler of `WithFailureHandler`; Podium logs it at ERROR.
`Failure()` returns that reason, and nil while the store serves (and after
`Close`); while it is not nil, Podium's `/readyz` answers 503 `storage failed`,
unless the node is still starting or shutting down, which it names first, and
the gRPC health service `NOT_SERVING` (`TestFailureNamesWhyTheStoreStopped`).
Successful writes survive a process restart without a shutdown save. Close waits
for an active transaction or snapshot. A database file lock excludes a
concurrent second process; opening uses a 250ms lock timeout (plus normal
scheduling overhead). The application closes the database on shutdown and
construction failures.

A snapshot starts once the log holds `cluster.standalone.snapshot_threshold`
entries (default 4096; env `SOLTI_CLUSTER_STANDALONE_SNAPSHOT_THRESHOLD`; zero
keeps the default, a negative value fails configuration loading) or 16 MiB of
them. A commit that crosses either wakes a background goroutine, so no
transaction runs a snapshot. It waits for the writer like a transaction, then
writes the published state as the snapshot at the last entry and deletes the
entries up to it, in one bbolt transaction. Transactions wait for it; reads do
not. A snapshot that fails makes the store unavailable, as a failed commit does.

Open checks the snapshot against its sha256 and decodes it, then applies the log
entries after it in index order, checking each CRC. A missing entry, an entry at
or below the snapshot index, a failed CRC, an undecodable command or an entry
that fails to apply stops startup with an error that names the entry; no entry
is skipped. The file stays as it was.

A file of the single-blob layout that earlier versions wrote, one
`control-plane-state` bucket holding the whole state, is migrated by the first
Open in one bbolt transaction: its snapshot, checked against its sha256 and
decoded, becomes the snapshot at index 0 in the current version, 4, with an
empty log, and the old bucket is deleted. A file that fails the checks is left
as it was. Earlier versions refuse the migrated file ("missing state bucket")
without changing it, so reverting after the migration needs a backup taken
before the upgrade. A file bbolt initialized and never committed to, which a
crash during the first Open leaves, opens as a fresh store; a file without
buckets that was committed to is refused.

A zero-byte `podium.db` holds no state. A crash while the very first Open
creates the file, before bbolt writes its first pages, can leave one, and Open
refuses it with an error that says so. If the deployment never started, move the
file aside, keeping it, and start again; otherwise restore a backup.

Snapshot encoding is shared with Raft to keep the entity schema consistent.
Decoding rejects invalid entities and duplicate IDs before replacing live state.
Snapshot writers use version 4, which adds derived agent credentials: a
generation and the ID of the enrollment secret in place of a token. Readers
accept version 3 losslessly: its ten entity groups are unchanged, and its agent
credentials, all of them with a token, stay stored ones. A single-blob file of
version 3 is migrated to a version-4 snapshot
(`TestDurableSnapshotV3UpgradePreservesTenGroups`). A version-3 snapshot that
holds a derived credential is rejected. Version 2 snapshots are rejected with
the version error, and legacy version 1 snapshots explicitly, rather than
decoded with the changed capabilities schema; automatic migration of either is
not implemented. A transaction costs what its callback reads and writes plus one
commit of its own operations; it no longer copies or rewrites the state. A
snapshot costs O(total state) serialization once per threshold. This backend is
appropriate for a single-node deployment; benchmark real state and write rates
before using it for a large/high-frequency fleet. Raft remains the existing HA
backend. Neither backend is a substitute for tested backup/restore.

For a consistent offline backup, stop Podium cleanly and copy `podium.db`;
restore it to a stopped deployment-owned directory with mode 0600. Do not copy a
live bbolt file with an ordinary file copy or delete the file to recover a
failed startup: retain the failing file and restore a verified backup instead.
The one case without a backup to restore is a zero-byte file on a deployment
that never started, above: move it aside and start again.

Raft recovery uses the same version-4 snapshot codec and versioned internal log
commands. New command writers use `SOLTI-CMD` followed by version byte 4.
Readers accept existing version-3 commands, but reject a derived agent
credential mislabeled as version 3, version-2 commands and unknown operations.
Old binaries reject version-4 commands/snapshots with their version error
instead of reading a derived credential as a stored one
(`TestV4StoreRefusedByV3Reader` shows every snapshot and entry this version
writes says version 4). Coordinate a cluster-wide upgrade; mixed v3/v4 binary
operation is unsupported. After a version-4 write, reverting the application
requires a compatible backup. Startup preflights retained snapshots and command
entries before starting the FSM. Legacy snapshots or headerless commands require
a compatible backup or offline migration; automatic migration is not
implemented. The log check first uses a read-only file handle, so rejected
legacy log bytes are not rewritten by database initialization. Runtime locks and
startup failures release all opened handles. Compatibility checks do not replace
functional validation of real backups on a separate disposable deployment.

Terminal execution records use a SHA-256 identity of length-prefixed agent ID,
task name and task UID plus exact generation and attempt integers. The first
complete observed result is immutable: duplicate and conflicting replays are
no-ops, including capture time, status, exit code, error and display metadata.
The archive survives deletion of its live agent/spec and participates in the
same atomic transaction, rollback, snapshot and Raft recovery mechanisms. It
stores bounded UTF-8 metadata and a 32-KiB error at most; it stores no command,
environment, credentials or logs. A scheduler-reported terminal result does not
prove that a canceled or timed-out process physically exited.

Collector retention limits apply to logical record payload, not database file
allocation or resident memory. Deleting rows reuses bbolt pages without
shrinking the file automatically. Offline backup/restore and real write-rate
benchmarks remain necessary for deployment.

## Crash safety

These are the claims the tests back, and how. The crash tests run the writes in
a child process that ends with `os.Exit` at a seam, which skips Close and every
deferred call; the parent then reads the file and reopens it.

- An acknowledged write survives the process ending without Close
  (`TestAcknowledgedWritesSurviveProcessExit`,
  `TestDurableExecutionArchiveSurvivesAbruptExit`).
- A command the process committed but did not apply before it ended is applied
  once on restart: the transaction creates a session and deletes an agent, which
  fails when applied twice, and the next write takes the next index
  (`TestWALReplaysCommittedCommandOnce`).
- A snapshot and the truncation of the log are one bbolt transaction. A process
  that ends after the snapshot write or after the truncation leaves the previous
  snapshot and the whole log; one that ends after the commit leaves the new
  snapshot and no entry up to it. Each reopens to the state the process
  acknowledged (`TestSnapshotTruncatesLog`).
- The migration is one bbolt transaction: a process that ends inside it leaves
  the single-blob file byte for byte, and the next Open migrates it
  (`TestExitDuringMigrationKeepsSingleBlob`). A single-blob file written by the
  previous version, checked in as `testdata/single-blob.db`, opens with the
  state it holds, which a write and a restart keep
  (`TestOpenMigratesSingleBlobStore`).
- A failed commit publishes nothing and makes the store unavailable until
  restart, which reads what the disk holds. The tests fail the commit by closing
  the bbolt handle under the store: fault injection on the commit path, not a
  failed sync (`TestRollbackCancellationAndCommitFailure`,
  `TestDurableExecutionCommitErrorDoesNotPublish`). A failed snapshot does the
  same (`TestFailedSnapshotFailsClosed`), and the failure handler hears either
  once, with the reason the next calls report
  (`TestFailureHandlerHearsEachFailureOnce`). A context canceled inside the
  commit's bbolt transaction rolls it back and leaves the store serving
  (`TestCancelInsideCommitChangesNothing`).
- A committed command that fails to apply makes the store unavailable with an
  error that names its log entry. The test removes the row the command deletes
  between the commit and the apply, so the fault is in memory only, and a
  restart applies the entry from the disk
  (`TestApplyFailureAfterCommitFailsClosed`); an entry that the store refuses on
  its own stops Open instead, below. One whose apply panics leaves the store
  unavailable once the panic is recovered, with a failure that names the entry
  (`TestApplyPanicFailsClosed`).
- Corrupt state stops Open and leaves the file as it was: a snapshot with a bad
  checksum, header, duplicate ID or no blob, a bad format or snapshot index, a
  missing, extra or every bucket deleted
  (`TestCorruptStateIsRejectedWithoutReset`); a log entry with a bad CRC or the
  CRC of another index, shorter than a CRC, with an undecodable or headerless
  command or an operation the store refuses, a gap, a missing first entry, an
  entry at or below the snapshot index, a key that is not an index and a bucket
  in the log (`TestOpenRejectsCorruptLogEntry`); a single-blob file with a bad
  checksum, header or duplicate ID, no snapshot, or next to a bucket of the log
  layout (`TestOpenRejectsCorruptSingleBlobStore`).
- A file bbolt initialized and never committed to opens as a fresh store
  (`TestOpenTreatsEmptyFileAsFresh`); a zero-byte or non-bbolt file is refused
  and kept, the zero-byte one with an error that says it holds no state and what
  to do (`TestInvalidExistingFileIsPreserved`).
- A transaction that records nothing begins no bbolt read transaction, writes no
  page and leaves the file's bytes as they were (`TestReadOnlyTxDoesNoIO`). A
  point read and a scan return, with the state before the commit, while a commit
  is held inside its bbolt transaction (`TestReadsDoNotWaitForFsync`), and while
  a snapshot is, which holds back a transaction
  (`TestReadsDoNotWaitForSnapshot`). Below the threshold no snapshot is due or
  written (`TestNoSnapshotBelowThreshold`). Close waits for a snapshot in
  progress, which commits whole (`TestCloseWaitsForSnapshot`).

No test models power loss, a failed or lying fsync, a torn write or a full disk.
That a commit survives power loss whole or not at all rests on bbolt, which
syncs the data pages before it writes and syncs the meta page that points at
them. Physical power loss, real device faults, sustained fleet load and
deployment-specific backup recovery remain separate checks.

## Performance

The benchmarks cover this store and the scans that run on top of it. They are in
`bench_test.go` here and in the history, lifecycle and spec service packages,
and share one fixture, `internal/storage/storagetest`: 20 specs, N agents and
optionally some history rows. An agent is a row with 6 metadata entries, 3
labels and 2 runners, plus a credential and 3 rollouts, about 900 B in a
snapshot. Run them in the Task container:

```sh
task golang:exec CMD='go test -run=^$ -bench=. -benchtime=2s ./internal/...'
```

`task ci/golang/bench` runs every benchmark once, so that they keep compiling
and running. It is not a performance gate.

### Final (P2a)

The final column is the median of 5 runs of `-benchtime=1s` from one session,
2026-10-31 00:53 to 00:57 UTC at commit `48b9ebb`, a p50 where the row says so
and the mean of one operation otherwise; the baseline is from an earlier session
on the same container type, the run before this work (Task 1) or, for a row
marked † that Task 1 did not have, the "before" of its own table, so a † factor
is better read in that table.

| benchmark | Task 1 baseline | final | factor |
| -- | -- | -- | -- |
| no-op tx p50, 5 000 agents | 67 ms | < 0.1 µs | > 670 000x |
| 1-agent tx p50, 5 000 agents | 71 ms | 0.86 ms | 83x |
| auth heartbeat p50, 5 000 agents | 139 ms | 0.87 ms | 160x |
| auth heartbeat commits/op, 5 000 agents | 2 (two transactions) | 1.000 | 2.0x |
| GetAgent during writes p50, 5 000 agents | 67 ms | 0.83 µs | 80 000x |
| ListPage | 5.99 ms / 8.7 MiB † | 0.70 ms / 0.21 MiB | 8.5x |
| Snapshot, 5 000 agents | - | 54 ms | - |
| LifecycleTick/stale100pct | 65 ms / 88 MiB | 6.1 ms / 8.7 MiB | 11x |
| HistoryPrune/10000/durable/idle | 47 ms / 52 MiB | 18 ns / 0 B | 2 600 000x |
| HistoryPrune/10000/raft/idle | 2.22 ms / 2.6 MiB † | 18 ns / 0 B | 120 000x |
| HistoryPrune/10000/durable/due | 7.6 ms / 5.5 MiB † | 4.3 ms / 2.8 MiB | 1.8x |
| HistoryPrune/10000/raft/due | 9.7 ms / 6.0 MiB † | 6.7 ms / 3.2 MiB | 1.5x |
| PlacementPreview/labels5000 | 75 ms / 100 MiB | 14 ms / 21 MiB | 5.3x |

### Transactions and reads

"Before" is the single-blob design, where every transaction copied and rewrote
the whole state: the p50 of one operation, the median of 10 runs of
`-benchtime=2s`. "After" is the log: the p50 of one operation, the median of 5
runs of `-benchtime=1s`, all taken in one session. An authenticated heartbeat is
two transactions: verify the token, then upsert the agent.
`GetAgent during writes` is a read while another goroutine commits one-agent
updates back to back.

| agents | snapshot | no-op tx | 1-agent tx | auth heartbeat | GetAgent during writes |
| -- | -- | -- | -- | -- | -- |
| 100 | 94 KiB | 3.9 ms → < 0.1 µs | 4.0 → 0.97 ms | 8.7 → 0.97 ms | 4.7 ms → 0.79 µs |
| 1 000 | 881 KiB | 16 ms → < 0.1 µs | 16 → 0.88 ms | 35 → 0.91 ms | 16 ms → 0.79 µs |
| 5 000 | 4.3 MiB | 67 ms → < 0.1 µs | 71 → 0.87 ms | 139 → 0.90 ms | 67 ms → 0.92 µs |

- A transaction that writes nothing touches no disk. Its p50 is below the 42 ns
  resolution of the benchmark's clock; its mean is 80 ns at every size.
- A one-agent transaction is one commit of one small entry, and the heartbeat's
  token check writes nothing, so a heartbeat is one commit too:
  `BenchmarkHeartbeat` reports 1.000 commits/op. Neither grows with the state: a
  commit writes the pages its entry touches, bbolt's freelist and its meta page,
  not the state.
- The old `GetAgent during writes` was not the cost of a read. The benchmark
  reads in a closed loop, and each read queued behind a whole commit of the
  writer, so it measured the commit. A read now waits for no commit, only for
  the apply of one.
- A snapshot (`BenchmarkSnapshot`: encode the state, write it and truncate the
  log in one commit) takes 3.1 ms at 100 agents, 13 ms at 1 000 and 56 ms at 5
  000, the mean of one operation, median of 5 runs. It runs once per threshold
  of commits, or 16 MiB of them, in the background; transactions wait for it,
  reads do not. The final session took 4.0 ms, 12 ms and 54 ms, the 100-agent
  runs from 2.9 to 5.5 ms.
- `HistoryPrune/10000/durable/due`, a retention pass that removes 100 rows in a
  transaction that scans 10 000, went from 30.4 ms, measured in an earlier
  session with the overlay and before the log, to 9.4 ms (5.8 MB allocated), the
  mean of one operation, median of 5 runs. `ListPage` reads as it did: 0.70 ms
  and 0.21 MiB in this session. The history counters took that pass on from
  there: see History retention.
- bbolt keeps an entry in the leaf at the end of the log and rewrites that leaf
  on each append until a split moves the entry off, about four entries later. So
  the few commits after a large transaction, such as the 4.3 MiB seed of the 5
  000-agent benchmarks, rewrite it too; the p50 does not show them.

### Scans

The scans run at 5 000 agents, except `labels1000`, which has 1 000, and the
history prune, which runs on a store of history rows alone. A prune finds
nothing to remove, a tick finds nothing due and a preview writes nothing, so
what they cost is the scan. The two `HistoryPrune` rows are the prune as it was
when it scanned the archive to decide; it no longer does (see History
retention). In `LifecycleTick`, `stale10pct` means that 10% of the agents are
past their staleness deadline. In `PlacementPreview`, `labels` is a spec that
selects every agent by label and `explicit20` one that lists 20 agents by ID.
Each time is the mean of one operation, taken as the median of 10 runs.

| benchmark | earlier run | before | after |
| -- | -- | -- | -- |
| HistoryPrune/1000/durable/idle | 0.70 ms / 0.5 MiB | 0.63 ms / 0.5 MiB | 0.32 ms / 0.3 MiB |
| HistoryPrune/10000/durable/idle | 47 ms / 52 MiB | 42 ms / 52 MiB | 2.2 ms / 2.6 MiB |
| LifecycleTick/stale0 | 0.11 ms / 40 KiB | 0.092 ms / 40 KiB | 0.096 ms / 40 KiB |
| LifecycleTick/stale10pct | 0.67 ms / 0.9 MiB | 0.60 ms / 0.9 MiB | 0.60 ms / 0.9 MiB |
| LifecycleTick/stale100pct | 65 ms / 88 MiB | 58 ms / 88 MiB | 5.8 ms / 8.7 MiB |
| PlacementPreview/explicit20 | 1.1 ms / 0.7 MiB | 0.98 ms / 0.7 MiB | 0.57 ms / 0.4 MiB |
| PlacementPreview/labels1000 | 4.3 ms / 5.8 MiB | 3.8 ms / 5.8 MiB | 2.7 ms / 4.0 MiB |
| PlacementPreview/labels5000 | 75 ms / 100 MiB | 67 ms / 100 MiB | 13 ms / 21 MiB |
| ListPage | - | 5.99 ms / 8.7 MiB | 0.74 ms / 0.21 MiB |

Each cell is the time and the memory allocated by one operation. "Earlier run"
is the baseline of the transaction table above, taken before this work. "Before"
and "after" ran in the same session on the same machine, the two `ListPage` runs
back to back, so read a factor against its own "before": that column came out
10% to 16% under the earlier run. The `HistoryPrune` rows were named
`HistoryPrune/{1000,10000}` then; they are the prune that finds nothing due on
the durable store.

For the scans, "after" is the single pass. Before it, the scans drained the
store page by page: `storage.CollectPages` asks for pages of 500, and each page
of an in-process store scanned, cloned and sorted all the rows that matched
again, so k rows took k/500 passes. Now `storage.AllAgents` and its siblings
take one pass from a store that implements `storage.Scanner`, as the in-memory,
durable and Raft stores do; the durable store holds its read lock once for the
pass, not once per page. `LifecycleTick/stale100pct` fell from 58 ms to 5.8 ms
(9.9x) and `HistoryPrune/10000/durable/idle` from 42 ms to 2.2 ms (19x). A scan
of one page or less (`stale0`, `stale10pct`) was a single pass already and costs
what it did.

`ListPage` is the first page of 100 of the 5 000 agents, asked for with a limit
as the API and the UI ask, which the single pass does not serve; it is the
median of 5 runs of `-benchtime=1s`. Its "before" and "after" are two versions
of `List`: the first cloned every agent to return 100 of them, the second sorts
the stored values and clones the page only. A page is still one scan and sort of
the matches, and so is each page of a paged scan.

### History retention

The history service keeps counters of its archive, its rows, their bytes and the
capture time of the oldest, so a retention pass that finds nothing due reads and
writes nothing. The benchmarks are
`HistoryPrune/10000/{durable,raft}/{idle,due}` in the history package's
`bench_test.go`: `idle` is a pass over 10 000 rows within every limit, and `due`
adds 100 rows that fall past retention before each pass, which removes them in
one transaction. "Before" is the pass that scanned the archive to decide,
"after" the pass that consults the counters, both in the same session on the
same machine, except that the two `idle` "after" cells come from the final
session (see Final (P2a)), measured again after a semaphore replaced a mutex on
that path. Each cell is the time and the memory allocated by one operation, the
median of 5 runs of `-benchtime=1s`.

| benchmark | before | after |
| -- | -- | -- |
| HistoryPrune/10000/durable/idle | 2.21 ms / 2.6 MiB | 18 ns / 0 B |
| HistoryPrune/10000/raft/idle | 2.22 ms / 2.6 MiB | 18 ns / 0 B |
| HistoryPrune/10000/durable/due | 7.6 ms / 5.5 MiB | 5.7 ms / 2.8 MiB |
| HistoryPrune/10000/raft/due | 9.7 ms / 6.0 MiB | 8.1 ms / 3.2 MiB |

- A due pass scans and sorts the archive once, in its transaction. Before, it
  did so twice, once to decide and once in the transaction.
- The counters are counted by one scan, when the service first decides, and
  again after a failed transaction, which may still have landed, and each time
  the node takes leadership, since another node may have written meanwhile.
  Between those, a capture scans only if it crosses a limit.
- The durable `due` pass took 9.4 ms in the session of the first table above and
  7.6 ms in this one, so read a factor against its own "before". The final
  session took the `due` passes at 4.3 ms (durable) and 6.7 ms (raft), 24% and
  18% under their "after" cells and with the same memory. The code of the passes
  has changed since only by that semaphore, so the difference is most likely the
  session.

### Conditions

These numbers come from the `task golang:exec` container on a busy development
machine, where the database file is on the container's overlay filesystem, and a
production disk will differ. Compare runs from the same machine and container
only. A synchronous 4 KiB write (`dd oflag=dsync`, which appends to a file) took
about 1.7 ms in the session of the transaction table's "before", and 1.0 ms
before and 1.25 ms after the runs of its "after"; a whole commit, with its two
bbolt syncs inside the file, took less than one such write in that session, so
`dd` does not give the cost of a commit there. Between the fastest and slowest
of the 10 "before" runs, most rows differ by 8% to 19%, and rows of a few
milliseconds or less by up to about 50%. Between those of the 5 "after" runs,
the timed cells of the transaction table differ by 0.1% to 16%, except the
1-agent transaction at 1 000 agents, where one run's p50 came out 1.28 ms
against 0.86 to 0.93 ms for the others (50%); the no-op's p50 does not differ.
The snapshots differ by 2% to 60%.
