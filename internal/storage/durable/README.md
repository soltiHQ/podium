# Durable standalone storage

Standalone uses durable storage by default in `data/podium`, relative to the
process working directory. For a stable absolute deployment location, configure:

```yaml
cluster:
  backend: standalone
  standalone:
    data_dir: /var/lib/solti/podium
```

Equivalent environment: `SOLTI_CLUSTER_STANDALONE_DATA_DIR=/var/lib/solti/podium`.
Use a deployment-owned local directory and keep it across process/container
restarts. Mount a persistent volume for a container deployment. Do not share this
directory between standalone replicas or switch a Raft data directory to it.
The startup log explicitly identifies persistent or ephemeral standalone storage.
For disposable development only, set `cluster.standalone.ephemeral: true` or
`SOLTI_CLUSTER_STANDALONE_EPHEMERAL=true`. This explicit flag takes precedence
and ignores DataDir, including the default. A blank DataDir without that flag
fails configuration validation instead of silently discarding restart state.

`podium.db` contains all storage entities, including credentials, sessions, task
specifications, rollout intent/state and agent enrollment tokens. New directories
use mode 0700; the database uses 0600. Existing directories and database files with group/world permissions or
symlinks, missing state and failed checksums are rejected without resetting state. No existing permissions or operator data are silently changed.
The existing event hub remains transient; this does not persist event/SSE history.

Each write stages a private copy, commits a versioned protobuf snapshot plus
SHA-256 checksum in a synced bbolt transaction, then publishes it to readers.
Queued reads and writes honor context deadlines through shared-read/exclusive-write
admission. Criteria-only filters do not wait for disk transactions. Callback
errors, cancellation observed before native commit and commit errors never
publish the candidate state. Once native commit starts, a successful fsync wins
concurrent cancellation and publishes the complete acknowledged snapshot; it is
not forcibly interrupted. Close waits for an active commit. A non-context commit error makes the store unavailable
until restart: fsync failure may leave the disk outcome uncertain, so continuing
from stale memory would be unsafe. Successful writes survive a process restart without a shutdown
save. A database file lock excludes a concurrent second process; opening uses a
250ms lock timeout (plus normal scheduling overhead).
The application closes the database on shutdown and construction failures.

Snapshot encoding is shared with Raft to keep the entity schema consistent.
Decoding rejects invalid entities and duplicate IDs before replacing live state.
Snapshot writers use version 3, which adds the bounded terminal execution archive.
Readers accept version 2 losslessly with an empty archive: its nine existing
entity groups and structured runner capabilities are unchanged. The next write
upgrades the snapshot to version 3. A version-2 snapshot containing archive rows
is rejected. Legacy version 1 snapshots are rejected explicitly rather than
decoded with the changed capabilities schema; automatic v1 migration is not
implemented.
Each transaction costs O(total state) serialization and copy work. This backend
is appropriate for a modest single-node deployment; benchmark real state and
write rates before using it for a large/high-frequency fleet. Raft remains the
existing HA backend. Neither backend is a substitute for tested backup/restore.

For a consistent offline backup, stop Podium cleanly and copy `podium.db`; restore
it to a stopped deployment-owned directory with mode 0600. Do not copy a live
bbolt file with an ordinary file copy or delete the file to recover a failed
startup: retain the failing file and restore a verified backup instead.

Raft recovery uses the same version-3 snapshot codec and versioned internal log
commands. New command writers use `SOLTI-CMD` followed by version byte 3. Readers
accept existing version-2 commands for the prior operations, but reject archive
operations mislabeled as version 2 and unknown operations. Old binaries reject
version-3 commands/snapshots instead of silently dropping archived history.
Coordinate a cluster-wide upgrade; mixed v2/v3 binary operation is unsupported.
After a version-3 write, reverting the application requires a compatible backup.
 Startup preflights retained snapshots and command entries before
starting the FSM. Legacy snapshots or headerless commands require a compatible
backup or offline migration; automatic migration is not implemented. The log
check first uses a read-only file handle, so rejected legacy log bytes are not
rewritten by database initialization. Runtime locks and startup failures release
all opened handles. Compatibility checks do not replace functional validation
of real backups on a separate disposable deployment.

Terminal execution records use a SHA-256 identity of length-prefixed agent ID,
task name and task UID plus exact generation and attempt integers. The first
complete observed result is immutable: duplicate and conflicting replays are
no-ops, including capture time, status, exit code, error and display metadata.
The archive survives deletion of its live agent/spec and participates in the
same atomic transaction, rollback, snapshot and Raft recovery mechanisms.
It stores bounded UTF-8 metadata and a 32-KiB error at most; it stores no command,
environment, credentials or logs. A scheduler-reported terminal result does not
prove that a canceled or timed-out process physically exited.

Collector retention limits apply to logical record payload, not database file
allocation or resident memory. Deleting rows reuses bbolt pages without shrinking
the file automatically. Offline backup/restore and real write-rate benchmarks
remain necessary for deployment. Local synthetic injection covers dirty-page,
data-sync, metadata-write and metadata-sync failures, short metadata writes and
corrupt snapshots. Three-node local Raft validation exercises archive state
through partition/rejoin and leader restart. Physical power loss, real device
faults, sustained fleet load and deployment-specific backup recovery remain
separate checks.
