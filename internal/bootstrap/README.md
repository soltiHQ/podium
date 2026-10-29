# First-start bootstrap

Bootstrap runs on every start of the node that seeds shared state: always in
standalone mode, the leader in raft mode. It reconciles the built-in roles and
seeds the first administrator.

## Built-in roles

The roles of `enum.BuiltinRoles` are reconciled on every start. A missing role is
created. A role whose name or permission set differs from the code definition is
reset to it, keeping its creation time. A role that already matches is not
written, and a custom role is never touched. Nothing edits roles at runtime, so
a difference comes from a new version of the definitions, and a new permission
reaches the administrator on upgrade.

## The administrator

`user-admin` (subject `admin`, administrator role) is seeded only when:

- the user table is empty, or
- `user-admin` is the only user and has no password credential. This is what a
  first start that stopped halfway leaves behind; the next start finishes it and
  keeps the user it finds.

With any other user table bootstrap touches no user and no credential, not even a
`user-admin` without a password next to other users: that is an established
system with a hand-edited database, and bootstrap does not mint a password for
it. An administrator the operator deleted stays deleted, and profile fields,
disabled state, role assignments and passwords survive every restart. Storage
errors abort bootstrap; they never delete, reset or replace existing state.

### Initial password

In order:

1. `SOLTI_BOOTSTRAP_ADMIN_PASSWORD`, when set.
   - It is env-only: the config field has `yaml:"-"`, so a YAML file cannot carry
     it. It is used as is, without trimming; an empty value counts as unset.
   - It must pass the password policy (12 characters at least, 72 bytes at most).
     Otherwise startup fails with an error that names the variable and never
     shows the value, and nothing is seeded.
   - It is never written to a file or a log. The log only says that the password
     was set from the variable.
   - A `bootstrap-admin-password` file that an earlier generated seed left in the
     data directory no longer matches the stored password. Bootstrap removes it
     once the new password is stored, never before, and logs the path. A file that
     cannot be removed only warns.
   - Once users exist the variable is ignored. One WARN says so, without the
     value, on every start, even for a value the policy would have refused. Remove
     it from the environment after the first start.
2. A generated password: 24 random bytes as 32 URL-safe characters, set through
   `credential.Service.SetPassword`, like any password.
   - **Persistent state** (every mode but ephemeral): it is written to
     `bootstrap-admin-password` in the data directory of the active backend,
     `cluster.standalone.data_dir` or `cluster.raft.data_dir`. The file holds the
     password on one line and has mode 0600. It goes to a temporary file in the
     same directory, which is synced and renamed over the target, so the file is
     never half written and a stale one from an earlier install is replaced whole.
     The log names only the absolute path: `bootstrap: admin password generated:
     read it from the file, log in and change it`, with `login` and `path`.
   - **Ephemeral mode** (`Options.Ephemeral`, which the app sets from
     `cluster.standalone.ephemeral: true`; there is no data directory): the
     password is logged once, at WARN, as `ephemeral mode: admin password
     generated: change it after first login`, with the `password` field. No other
     mode ever logs a password.
   - **Neither**: bootstrap does not guess ephemeral mode from an empty data
     directory. With no data directory and no `Ephemeral` flag a seed fails with
     `ErrNoDataDir` before anything is stored, and nothing is logged. A data
     directory wins when both are given.

The file is written before the password is stored. A start that stops in between
leaves a file nobody needs, never a stored password nobody can read. A start that
fails while storing keeps its file too, and its error names the file's absolute
path, never the password: a write can fail after it took effect (a raft apply
that loses leadership once its entry committed, a failed fsync), and then the
file is the only readable copy of the stored password. If nothing was stored,
the next start seeds again and replaces the file whole; if the password was
stored, the next start leaves the file alone. A restart with nothing to seed
never touches an existing file; delete it once the password is changed.

In raft mode the leader seeds, so the file sits in the raft `data_dir` of the node
that led at first boot. The other replicas do not have it.

## Tests

`bootstrap_test.go` covers:

- the password file: mode, login with its contents, no rewrite on restart,
  replacement of a stale file, no stored password without a written file, the
  file kept after a failed or uncertain write and named in its error, and its
  absolute path in the log, also for a relative data directory;
- a deleted `user-admin` that stays deleted, the first start that stopped
  halfway, and `user-admin` without a password next to other users, which is left
  alone wherever it sits in the user list;
- the password variable: use, policy violation, the ignored-variable WARN, and the
  removal of a stale password file only after the new password is stored;
- no password in the log, persistent or from the environment, the ephemeral log
  line, and the refusal to seed without a data directory or the ephemeral flag;
- the reconciliation of built-in roles, and the earlier checks of an existing
  operator-managed administrator and a canceled startup.
