-- podium's schema, version 2: the catalog of workload schemas, one row per
-- schema fingerprint. The row is the fingerprint, the time the schema was
-- first seen and the body, the protojson of the snapshot message, which
-- carries every field. It only adds a table: the podium of version 1 runs on
-- the schema this file leaves.

CREATE TABLE podium_workload_schemas (
    id         bytea  PRIMARY KEY, -- the fingerprint
    created_at bigint NOT NULL,    -- the time the schema was first seen
    body       bytea  NOT NULL
);
CREATE INDEX podium_workload_schemas_listing ON podium_workload_schemas (created_at DESC, id);

-- The version this file brings the schema to: the last statement of every
-- migration file, see migrations/README.md.
INSERT INTO podium_schema_migrations (version) VALUES (2) ON CONFLICT (version) DO NOTHING;
