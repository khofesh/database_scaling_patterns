-- Schema lives only on the primary. Replicas receive it automatically through
-- streaming replication (the WAL carries the CREATE TABLE/INSERT records), so we
-- never run DDL against a replica.

CREATE TABLE products (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(200) NOT NULL,
    price       DECIMAL(10, 2) NOT NULL,
    stock       INTEGER NOT NULL DEFAULT 0,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_products_name ON products (name);

-- Seed a few rows so replicas have data immediately after the base backup.
INSERT INTO products (name, price, stock) VALUES
    ('Mechanical Keyboard', 89.99, 120),
    ('Wireless Mouse',      29.50, 300),
    ('27-inch Monitor',     219.00, 45),
    ('USB-C Hub',           39.99, 200);
