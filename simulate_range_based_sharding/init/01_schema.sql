-- Identical schema on every shard. Which ROWS a shard holds is decided by the
-- application's range map (created_at ranges), not by anything in the database.

CREATE TABLE orders (
    -- uuidv7() (new in PostgreSQL 18) is globally unique AND time-ordered, so ids
    -- never collide across shards and still index well (no random B-tree inserts).
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    customer_id INT  NOT NULL,
    amount      NUMERIC(10, 2) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);

-- The shard key. Range queries ("orders in March") become index range scans.
CREATE INDEX orders_created_at_idx ON orders (created_at);
