-- WRITE MODEL: normalized. Separate tables, foreign keys, no redundancy.
-- This shape is great for writes (one fact in one place) but needs JOINs to read.

CREATE TABLE customers (
    id    SERIAL PRIMARY KEY,
    name  TEXT NOT NULL,
    email TEXT NOT NULL UNIQUE
);

CREATE TABLE orders (
    id          SERIAL PRIMARY KEY,
    customer_id INT NOT NULL REFERENCES customers(id),
    status      TEXT NOT NULL DEFAULT 'placed',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE order_items (
    id        SERIAL PRIMARY KEY,
    order_id  INT NOT NULL REFERENCES orders(id),
    product   TEXT NOT NULL,
    qty       INT NOT NULL,
    price_cents INT NOT NULL
);

-- OUTBOX: every state change appends an event here in the same transaction as the
-- write. The projector polls this table and updates the read model. Writing the
-- event transactionally with the data is what makes the projection reliable.
CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    aggregate  TEXT NOT NULL,   -- e.g. 'order'
    payload    JSONB NOT NULL,
    processed  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_events_unprocessed ON events (id) WHERE NOT processed;

-- Seed a couple of customers to place orders for.
INSERT INTO customers (name, email) VALUES
    ('Alice Wong', 'alice@example.com'),
    ('Bob Singh', 'bob@example.com');
