-- The DIRECTORY (lookup service): the single source of truth for "which shard holds
-- tenant X". Routing is a lookup, not a formula, so any tenant can live anywhere and
-- can be moved by changing one row.

CREATE TABLE shards (
    name     TEXT PRIMARY KEY,
    dsn      TEXT NOT NULL,
    -- 'open' shards accept newly onboarded tenants; 'closed' ones only serve existing.
    status   TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed'))
);

CREATE TABLE tenant_directory (
    tenant_id  TEXT PRIMARY KEY,
    shard      TEXT NOT NULL REFERENCES shards (name),
    -- 'moving' while a migration is in progress: writers must wait or retry.
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'moving')),
    -- bumped on every move, so caches can tell an old mapping from a new one
    version    INT  NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON tenant_directory (shard);

-- Shards 1 and 2 exist from the start; shard3 is registered later by the demo to
-- show adding capacity without rehashing anybody.
INSERT INTO shards (name, dsn) VALUES
    ('shard1', 'postgres://postgres:postgres@localhost:5511/app?sslmode=disable'),
    ('shard2', 'postgres://postgres:postgres@localhost:5512/app?sslmode=disable');
