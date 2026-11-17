# Migrations

podium's SQL migrations: the schema of the database mode, one file per version,
in `postgres/`. podium applies them itself, with `podium migrate` or at each
replica's start (`cluster.database.migrate: true`), and a tool of the
deployment's own, as Bytebase, can apply the same files instead. The package
embeds them (`migrations.Postgres()`), and `internal/storage/sqlstore` runs
them.

## The files

- `postgres/NNNN_name.sql`: the version first, four digits, then a name of
  lower-case letters, digits and `_`, versions 1, 2, 3… without a gap. A file of
  another name, or a gap, stops the migrator.

- Each file's last statement records its version, on a line of its own, and no
  other statement of the file inserts into `podium_schema_migrations`:

  ```sql
  INSERT INTO podium_schema_migrations (version) VALUES (1) ON CONFLICT (version) DO NOTHING;
  ```

  podium's migrator records nothing itself: whoever applies a file, the database
  ends at its version, which podium reads. As the statement comes last, a tool
  that stops at the first failing statement records a version only with the
  whole file applied. The migrator refuses a file without the statement, and
  rolls back a file that leaves the schema at another version than its own.

- `0001_init.sql` creates `podium_schema_migrations` first, with the statement
  the migrator runs before it, which a test keeps alike.

- `0002_workload_schemas.sql` adds `podium_workload_schemas`, the catalog of
  workload schemas, one row per fingerprint. It only adds a table, so the
  previous podium runs on the schema it leaves.

- A file runs in one transaction, whoever applies it: no `BEGIN`, `COMMIT`,
  `SAVEPOINT` or other transaction control, and nothing that runs outside a
  transaction, as `CREATE INDEX CONCURRENTLY` or `VACUUM`.

- A migration only adds: tables, nullable columns or columns with a default,
  indexes. During a rolling update the podium before it runs on the schema it
  leaves, so no drop, truncation, deletion, rename or type change, and no column
  made `NOT NULL`, or added `NOT NULL` without a default.

- Forward only: there is no down migration. A database whose schema is newer
  than a replica's stops that replica, as after a rollback past a migration;
  only a backup goes back.

- A migration in a release never changes, and none is removed: a database it
  migrated holds its version and never runs it again, and Bytebase flags a
  changed file that it applied. A change of the schema is a new file.

`internal/storage/sqlstore` checks the names, the versions and the statement
that records the version each time it reads the files. The tests of this package
check the rest on podium's own files: `checks_test.go` reads their statements as
words, refuses the ones above, and cannot read dollar quoting; `checksums` in
`migrations_test.go` holds the SHA-256 sum of each file, which a new migration
extends and which a released one never changes.

## Applying them with a tool of your own

podium can leave its schema to the tool that migrates your databases, as
Bytebase:

1. Take the files of the podium version you deploy. `podium schema export`
   writes them out, here from the image:

   ```sh
   docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/podium:/out" \
     ghcr.io/soltihq/podium:<version> schema export --dir /out
   ```

   It keeps a file of the directory with the same content, and stops before it
   writes anything at a file of a migration's name with other content.
   `podium schema version` prints the schema version that podium runs on. The
   files are also in the repository at the release's tag, in
   `migrations/postgres`.

1. Let the tool apply them in version order, each in a transaction; Bytebase
   takes a file's version from the leading digits of its name. Migrate the
   database before the podium version that needs it rolls out: until then, its
   replicas refuse to start, while the old ones serve.

1. Run podium with `cluster.database.migrate: false`
   (`SOLTI_CLUSTER_DATABASE_MIGRATE=false`; the chart's
   `migrate.mode: external`). A replica then migrates nothing, and exits at
   start on a schema older than its own, naming both versions, as it does on a
   newer one.

One database has one migrator: the tool keeps a record of what it applied, which
`podium migrate` does not update. Drop the schema with that tool too, not with
`podium drop-schema` or the chart's `dropSchema.onUninstall`.

### A role that only reads and writes rows

With the schema migrated by another role, podium runs on a role that only reads
and writes rows. As the role that migrates, which owns the tables:

```sql
CREATE ROLE podium_app LOGIN PASSWORD '<password>';
GRANT CONNECT ON DATABASE podium TO podium_app;
GRANT USAGE ON SCHEMA public TO podium_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO podium_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO podium_app;
```

The default privileges cover the tables that later migrations make, as long as
the same role makes them; run by another role, the statement names the owner,
`ALTER DEFAULT PRIVILEGES FOR ROLE <owner> …`. In a schema other than `public`,
name it, and set the role's `search_path` to it. podium needs nothing on a
sequence: `podium_events.seq` is an identity column. The tests run the storage
contract and two replicas, with their lease, registry, fence, events and
notifications, on such a role (`pgtest.DMLRole`, `TestContract`,
`TestDatabaseRunsOnARoleThatOnlyReadsAndWritesRows`). `podium migrate` and
`podium drop-schema` need the owner.
