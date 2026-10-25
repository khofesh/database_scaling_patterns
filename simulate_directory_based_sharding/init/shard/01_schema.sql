-- Identical schema on every shard. A shard knows nothing about the directory; it just
-- stores rows for the tenants placed on it.

-- Which tenants this shard is currently authoritative for. Writes check it, so a
-- router holding a STALE directory entry can't silently write to the old shard
-- after a tenant has moved (a simple fencing mechanism).
CREATE TABLE hosted_tenants (
    tenant_id TEXT PRIMARY KEY
);

CREATE TABLE orders (
    tenant_id  TEXT   NOT NULL,
    id         BIGINT NOT NULL,
    amount     NUMERIC(10, 2) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
