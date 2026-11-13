-- podium's schema, version 1: the entities, the database leadership's lease
-- and replica registry, the events every replica applies, and the record of
-- the migrations applied.

-- podium_schema_migrations records each migration applied. The migrator
-- creates it with this same statement before it applies this file, as it
-- locks the table first; the statement is repeated here so that this file
-- holds the whole schema, and a test keeps the two alike.
CREATE TABLE IF NOT EXISTS podium_schema_migrations (
    version    integer     PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

-- The entities, a table per entity group. A row is the entity's ID, its
-- creation time, the columns its lookups, criteria and counts read, and its
-- body: the protojson of the entity's snapshot message, which carries every
-- field. The other columns only index it.
--
-- An entity's strings are bytea: bytea compares and orders by bytes, as Go
-- compares strings, with no collation, and carries a NUL, which text refuses.
-- The body is bytea too: jsonb refuses the escape \u0000 protojson writes for
-- a NUL. A time a statement compares is a bigint of Unix nanoseconds, 0 for
-- the zero time. A status podium fills from its own enum is a smallint, or
-- text. No constraint checks more than the store does in Go, but for the size
-- of an index entry, near 2.7 kB, which the store checks first (2048 bytes a
-- key): a lookup's column is indexed, not unique, as in the in-memory store.
--
-- Every listing runs in the order (created_at DESC, id ASC), which the
-- table's _listing index holds.

CREATE TABLE podium_agents (
    id         bytea    PRIMARY KEY,
    created_at bigint   NOT NULL,
    status     smallint NOT NULL, -- enum.AgentStatus
    stale_at   bigint   NOT NULL, -- the staleness deadline; 0: never set
    body       bytea    NOT NULL
);
CREATE INDEX podium_agents_listing ON podium_agents (created_at DESC, id);

-- One credential per agent, keyed by the agent's ID.
CREATE TABLE podium_agent_credentials (
    id         bytea  PRIMARY KEY,
    created_at bigint NOT NULL,
    body       bytea  NOT NULL
);
CREATE INDEX podium_agent_credentials_listing ON podium_agent_credentials (created_at DESC, id);

CREATE TABLE podium_users (
    id         bytea  PRIMARY KEY,
    created_at bigint NOT NULL,
    subject    bytea  NOT NULL,
    body       bytea  NOT NULL
);
CREATE INDEX podium_users_listing ON podium_users (created_at DESC, id);
CREATE INDEX podium_users_subject ON podium_users (subject);

CREATE TABLE podium_credentials (
    id         bytea  PRIMARY KEY,
    created_at bigint NOT NULL,
    user_id    bytea  NOT NULL,
    auth       bytea  NOT NULL, -- enum.Auth, which a credential may leave empty
    body       bytea  NOT NULL
);
CREATE INDEX podium_credentials_listing ON podium_credentials (created_at DESC, id);
CREATE INDEX podium_credentials_user ON podium_credentials (user_id, auth);

CREATE TABLE podium_verifiers (
    id            bytea  PRIMARY KEY,
    created_at    bigint NOT NULL,
    credential_id bytea  NOT NULL,
    body          bytea  NOT NULL
);
CREATE INDEX podium_verifiers_listing ON podium_verifiers (created_at DESC, id);
CREATE INDEX podium_verifiers_credential ON podium_verifiers (credential_id);

CREATE TABLE podium_sessions (
    id         bytea  PRIMARY KEY,
    created_at bigint NOT NULL,
    user_id    bytea  NOT NULL,
    body       bytea  NOT NULL
);
CREATE INDEX podium_sessions_listing ON podium_sessions (created_at DESC, id);
CREATE INDEX podium_sessions_user ON podium_sessions (user_id);

CREATE TABLE podium_roles (
    id         bytea  PRIMARY KEY,
    created_at bigint NOT NULL,
    name       bytea  NOT NULL,
    body       bytea  NOT NULL
);
CREATE INDEX podium_roles_listing ON podium_roles (created_at DESC, id);
CREATE INDEX podium_roles_name ON podium_roles (name);

CREATE TABLE podium_specs (
    id         bytea  PRIMARY KEY,
    created_at bigint NOT NULL,
    body       bytea  NOT NULL
);
CREATE INDEX podium_specs_listing ON podium_specs (created_at DESC, id);

CREATE TABLE podium_rollouts (
    id         bytea    PRIMARY KEY,
    created_at bigint   NOT NULL,
    spec_id    bytea    NOT NULL,
    agent_id   bytea    NOT NULL,
    status     smallint NOT NULL, -- enum.SyncStatus
    body       bytea    NOT NULL
);
CREATE INDEX podium_rollouts_listing ON podium_rollouts (created_at DESC, id);
CREATE INDEX podium_rollouts_spec ON podium_rollouts (spec_id);
CREATE INDEX podium_rollouts_agent ON podium_rollouts (agent_id);

CREATE TABLE podium_executions (
    id         bytea  PRIMARY KEY,
    created_at bigint NOT NULL, -- the capture time
    agent_id   bytea  NOT NULL,
    spec_id    bytea  NOT NULL,
    task_name  bytea  NOT NULL,
    task_uid   bytea  NOT NULL,
    status     text   NOT NULL, -- a terminal status, as the record's constructor allows it
    body       bytea  NOT NULL
);
CREATE INDEX podium_executions_listing ON podium_executions (created_at DESC, id);
CREATE INDEX podium_executions_agent ON podium_executions (agent_id);
CREATE INDEX podium_executions_spec ON podium_executions (spec_id);

-- The database leadership's lease: one row, taken when it is free or expired
-- by a statement that raises epoch, and renewed by its holder. Every write
-- transaction fences itself on epoch and holder. expires_at is in the
-- database's clock.
CREATE TABLE podium_leader (
    id         smallint    PRIMARY KEY CHECK (id = 1),
    epoch      bigint      NOT NULL,
    holder     text        NOT NULL, -- the holding replica's ID
    address    text        NOT NULL, -- where the others forward writes: its advertise address
    expires_at timestamptz NOT NULL
);

-- The replica registry: each replica heartbeats its row, with the digests of
-- the settings every replica must share. last_seen is in the database's clock.
CREATE TABLE podium_replicas (
    id             text        PRIMARY KEY, -- random, per process
    advertise_addr text        NOT NULL,
    digests        bytea       NOT NULL, -- cluster.Digests, the groups in order
    last_seen      timestamptz NOT NULL
);

-- The events every replica applies: each a row in sequence, read after the
-- last sequence a replica applied, and pruned by age.
CREATE TABLE podium_events (
    seq        bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       text        NOT NULL,
    payload    bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX podium_events_created_at ON podium_events (created_at);
