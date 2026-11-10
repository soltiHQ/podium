# First-start bootstrap

Bootstrap reconciles the built-in roles and seeds the first administrator.
Standalone runs it once per start, inside `app.New`: a seed that fails fails the
start, and a generated password is in its file when `New` returns. A raft node
runs it at the start of each of its leadership terms, see "Raft mode" below.

## Built-in roles

The roles of `enum.BuiltinRoles` are reconciled on every standalone start, and
in raft mode at the start of every leadership term, to the code definitions of
the node that leads. A missing role is created. A role whose name or permission
set differs from the code definition is reset to it, keeping its creation time.
A role that already matches is not written, and a custom role is never touched.
Nothing edits roles at runtime, so a difference comes from a new version of the
definitions, and a new permission reaches the administrator on upgrade. During a
rolling upgrade of a raft cluster the roles follow whichever version leads: a
term of an upgraded node gives them the new definitions, and a later term of a
node not yet upgraded resets them to the old ones, until every node runs the new
version.

## The administrator

`user-admin` (subject `admin`, administrator role) is seeded only when:

- the user table is empty, or
- `user-admin` is the only user and has no password credential. This is what a
  first start that stopped halfway leaves behind; the next start, or in raft
  mode the next run, finishes it and keeps the user it finds.

With any other user table bootstrap touches no user and no credential, not even
a `user-admin` without a password next to other users: that is an established
system with a hand-edited database, and bootstrap does not mint a password for
it. An administrator the operator deleted stays deleted, and profile fields,
disabled state, role assignments and passwords survive every restart and every
leadership term. Storage errors abort bootstrap; they never delete, reset or
replace existing state.

### Initial password

In order:

1. `SOLTI_BOOTSTRAP_ADMIN_PASSWORD`, when set.
   - It is env-only: the config field has `yaml:"-"`, so a YAML file cannot
     carry it. It is used as is, without trimming; an empty value counts as
     unset.
   - It must pass the password policy (12 characters at least, 72 bytes at
     most). Otherwise startup fails with an error that names the variable and
     never shows the value, and nothing is seeded. Standalone checks it when a
     seed is due. A raft node checks it at start, before anything starts,
     whatever the user table holds: it cannot know then whether a term of its
     own will seed.
   - It is never written to a file or a log. The log only says that the password
     was set from the variable.
   - A `bootstrap-admin-password` file that an earlier generated seed left in
     the data directory no longer matches the stored password. Bootstrap removes
     it once the new password is stored, never before, and logs the path. A file
     that cannot be removed only warns.
   - Once users exist the variable is ignored. One WARN says so, without the
     value, on every standalone start, even for a value the policy would have
     refused, and at the start of every leadership term of a raft node, which
     refuses such a value at start. Remove it from the environment after the
     first start.
1. A generated password: 24 random bytes as 32 URL-safe characters, set through
   `credential.Service.SetPassword`, like any password.
   - **Persistent state** (every mode but ephemeral): it is written to
     `bootstrap-admin-password` in the data directory of the active backend,
     `cluster.standalone.data_dir` or `cluster.raft.data_dir`. The file holds
     the password on one line and has mode 0600. It goes to a temporary file in
     the same directory, which is synced and renamed over the target, so the
     file is never half written and a stale one from an earlier install is
     replaced whole. The log names only the absolute path:
     `bootstrap: admin password generated: read it from the file, log in and change it`,
     with `login` and `path`, and in raft mode the `node_id` of the node that
     wrote the file.
   - **Ephemeral mode** (`Options.Ephemeral`, which the app sets from
     `cluster.standalone.ephemeral: true`; there is no data directory): the
     password is logged once, at WARN, as
     `ephemeral mode: admin password generated: change it after first login`,
     with the `password` field. No other mode ever logs a password.
   - **Neither**: bootstrap does not guess ephemeral mode from an empty data
     directory. With no data directory and no `Ephemeral` flag a seed fails with
     `ErrNoDataDir` before anything is stored, and no password is logged. A data
     directory wins when both are given.

The file is written before the password is stored. A start that stops in between
leaves a file nobody needs, never a stored password nobody can read. A start
that fails while storing keeps its file too, and its error names the file's
absolute path, never the password: a write can fail after it took effect (a raft
apply that loses leadership once its entry committed, a failed fsync), and then
the file is the only readable copy of the stored password. If nothing was
stored, the next start, or in raft mode the next run, seeds again and replaces
the file whole; if the password was stored, it leaves the file alone. A restart
with nothing to seed never touches an existing file; delete it once the password
is changed.

## Raft mode

A raft node does not wait for a leader at start. `app.New` checks what any seed
needs before anything starts (`Options.Check`): a data directory, which a raft
node always has (`ErrNoDataDir` otherwise), and the password policy for a set
`SOLTI_BOOTSTRAP_ADMIN_PASSWORD`. Then the node's `bootstrap` runner, a term
task of `runner/base`, runs the whole bootstrap at the start of each leadership
term of the node, once the term's barrier saw every committed write applied. A
quorum that forms long after its nodes started still seeds, at its first
leadership gain. A later term finds the administrator seeded and changes
nothing.

- A run that fails while the node leads logs one WARN,
  `runner task failed: retrying`, with the error, and runs again after a pause
  that starts at a second and doubles after each failure, up to a minute.
- A run that ends with its term, a leadership loss, is not retried: one INFO,
  `runner task ended with its leadership term: the next term runs it again`, and
  the next term, on whichever node leads, runs bootstrap again.
- The runner and bootstrap log with the node's `node_id`, so the line that names
  a password file names the node that holds it (`TestLateQuorumStillSeedsOnce`).

Each node writes a password it generates into its own `cluster.raft.data_dir`,
and never removes the file of another node. So the file is on the node whose
term generated the password: the one that logged `admin password generated`,
with its `node_id` and the file's absolute `path`. A seed can fail on one leader
after it wrote its file and complete on the next leader, with a password of its
own; the first file is stale then. Trust the file of the node that logged
`admin password generated`. If no node did and the administrator exists all the
same, the failed write took effect (raft can report a lost leadership once the
entry committed), and the next leader found nothing to seed: trust the file of
the node whose seed failed after writing it, whose error names the file.

## Tests

`bootstrap_test.go` covers:

- the password file: mode, login with its contents, no rewrite on restart,
  replacement of a stale file, no stored password without a written file, the
  file kept after a failed or uncertain write and named in its error, and its
  absolute path in the log, also for a relative data directory;
- a deleted `user-admin` that stays deleted, the first start that stopped
  halfway, and `user-admin` without a password next to other users, which is
  left alone wherever it sits in the user list;
- the password variable: use, policy violation, the ignored-variable WARN, and
  the removal of a stale password file only after the new password is stored;
- no password in the log, persistent or from the environment, the ephemeral log
  line, and the refusal to seed without a data directory or the ephemeral flag;
- `Options.Check`, the checks of a raft node at start
  (`TestCheckRefusesWhatEverySeedNeeds`);
- the reconciliation of built-in roles, and the earlier checks of an existing
  operator-managed administrator and a canceled startup.

Raft mode is tested in `internal/app` on three real raft nodes with mutual TLS,
started through `app.New` and `Run`: a quorum that forms late seeds one
administrator, with the same stored password on every node, which the file this
README says to trust holds (`TestLateQuorumStillSeedsOnce`), and a new leader's
term changes nothing (`TestSeedingIsIdempotentAcrossLeaders`). The checks at
start are `TestNewRaftRefusesAnAdminPasswordBreakingThePolicy` and
`TestNewRaftRefusesToStartWithoutADataDirectory`.
