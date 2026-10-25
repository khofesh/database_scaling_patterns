-- PostgreSQL is the source of truth. Redis and Elasticsearch hold DERIVED copies,
-- fed from the transactional outbox below.

CREATE TABLE products (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL,
    category    TEXT NOT NULL,
    price       NUMERIC(10, 2) NOT NULL,
    stock       INT NOT NULL,
    -- incremented on every change; downstream stores use it to ignore stale or
    -- duplicate updates (Elasticsearch external versioning)
    version     BIGINT NOT NULL DEFAULT 1
);

CREATE TABLE orders (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    product_id BIGINT NOT NULL REFERENCES products (id),
    qty        INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- TRANSACTIONAL OUTBOX: written in the SAME transaction as the business change, so
-- an event exists if and only if the change committed. A relay process reads it
-- and updates the other stores — no dual-write to three systems from the app.
CREATE TABLE outbox (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event        TEXT NOT NULL,              -- 'product_upserted' | 'order_placed'
    payload      JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);
-- The relay only ever looks for unprocessed events.
CREATE INDEX outbox_pending_idx ON outbox (id) WHERE processed_at IS NULL;
