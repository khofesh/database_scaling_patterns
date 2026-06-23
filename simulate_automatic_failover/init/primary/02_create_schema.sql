-- Created on the primary only; replicated to the standbys via WAL.

CREATE TABLE customers (
    id         SERIAL PRIMARY KEY,
    name       VARCHAR(150) NOT NULL,
    email      VARCHAR(255) NOT NULL,
    tier       VARCHAR(20)  NOT NULL DEFAULT 'standard',
    created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_customers_tier ON customers (tier);

INSERT INTO customers (name, email, tier) VALUES
    ('Alice Johnson', 'alice@example.com', 'premium'),
    ('Bob Smith',     'bob@example.com',   'standard'),
    ('Carol White',   'carol@example.com', 'premium'),
    ('Dan Brown',     'dan@example.com',   'standard');
