-- DENORMALIZATION: deliberately store redundant copies of data so hot reads avoid
-- joins and aggregates. The normalized tables stay the source of truth; the copies
-- are derived and must be kept consistent (here: with triggers).

-- ── Normalized model (3NF) ───────────────────────────────────────────────────
CREATE TABLE customers (
    id    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name  TEXT NOT NULL,
    email TEXT NOT NULL UNIQUE
);

CREATE TABLE products (
    id    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name  TEXT NOT NULL,
    price NUMERIC(10, 2) NOT NULL
);

CREATE TABLE orders (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id BIGINT NOT NULL REFERENCES customers (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- ── Denormalized (redundant) columns ──
    customer_name TEXT,                          -- copy of customers.name
    item_count    INT           NOT NULL DEFAULT 0,  -- count of order_items
    total_amount  NUMERIC(12, 2) NOT NULL DEFAULT 0  -- sum(qty * unit_price)
);
CREATE INDEX ON orders (customer_id, created_at DESC);
CREATE INDEX ON orders (created_at DESC);

CREATE TABLE order_items (
    order_id   BIGINT NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    product_id BIGINT NOT NULL REFERENCES products (id),
    qty        INT NOT NULL CHECK (qty > 0),
    unit_price NUMERIC(10, 2) NOT NULL,
    PRIMARY KEY (order_id, product_id)
);

-- ── Seed: 2,000 customers, 500 products, 200,000 orders, ~600,000 items ──────
INSERT INTO customers (name, email)
SELECT 'Customer ' || g, 'c' || g || '@example.com' FROM generate_series(1, 2000) g;

INSERT INTO products (name, price)
SELECT 'Product ' || g, round((random() * 90 + 10)::numeric, 2) FROM generate_series(1, 500) g;

-- Customer 1 is a "whale" with many orders (makes the rename cost visible).
INSERT INTO orders (customer_id, created_at)
SELECT CASE WHEN g % 20 = 0 THEN 1 ELSE 1 + (g % 2000) END,
       now() - random() * interval '365 days'
FROM generate_series(1, 200000) g;

INSERT INTO order_items (order_id, product_id, qty, unit_price)
SELECT o.id, p.id, 1 + (random() * 4)::int, p.price
FROM orders o
CROSS JOIN LATERAL (
    SELECT id, price FROM products
    WHERE id IN (1 + (o.id * 7) % 500, 1 + (o.id * 13) % 500, 1 + (o.id * 31) % 500)
) p;

-- Backfill the redundant columns once, set-based, before the triggers exist.
UPDATE orders o SET customer_name = c.name FROM customers c WHERE c.id = o.customer_id;
UPDATE orders o
SET item_count = t.n, total_amount = t.total
FROM (SELECT order_id, count(*) AS n, sum(qty * unit_price) AS total
      FROM order_items GROUP BY order_id) t
WHERE t.order_id = o.id;

-- ── Consistency triggers ─────────────────────────────────────────────────────

-- 1. New orders copy the customer's current name.
CREATE FUNCTION orders_set_customer_name() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    SELECT name INTO NEW.customer_name FROM customers WHERE id = NEW.customer_id;
    RETURN NEW;
END $$;
CREATE TRIGGER orders_customer_name BEFORE INSERT OR UPDATE OF customer_id ON orders
    FOR EACH ROW EXECUTE FUNCTION orders_set_customer_name();

-- 2. Renaming a customer fans out to every one of their orders. This is the hidden
--    WRITE cost of denormalization: one logical change, N physical row updates.
CREATE FUNCTION customers_propagate_name() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE orders SET customer_name = NEW.name WHERE customer_id = NEW.id;
    RETURN NULL;
END $$;
CREATE TRIGGER customers_name_fanout AFTER UPDATE OF name ON customers
    FOR EACH ROW WHEN (OLD.name IS DISTINCT FROM NEW.name)
    EXECUTE FUNCTION customers_propagate_name();

-- 3. Item changes adjust the order's count and total. STATEMENT-level triggers with
--    transition tables see all affected rows at once, so a bulk insert of 1,000
--    items does one grouped UPDATE instead of 1,000 single-row updates.
--    PL/pgSQL plans each statement lazily, so a branch may reference a transition
--    table that only exists for that event.
CREATE FUNCTION order_items_recalc() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE orders o SET item_count = o.item_count + d.n, total_amount = o.total_amount + d.total
        FROM (SELECT order_id, count(*) AS n, sum(qty * unit_price) AS total
              FROM new_rows GROUP BY order_id) d
        WHERE o.id = d.order_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE orders o SET item_count = o.item_count - d.n, total_amount = o.total_amount - d.total
        FROM (SELECT order_id, count(*) AS n, sum(qty * unit_price) AS total
              FROM old_rows GROUP BY order_id) d
        WHERE o.id = d.order_id;
    ELSE -- UPDATE: subtract the old version, add the new one
        UPDATE orders o SET item_count = o.item_count + d.n, total_amount = o.total_amount + d.total
        FROM (SELECT order_id, sum(n) AS n, sum(total) AS total FROM (
                  SELECT order_id, 1 AS n, qty * unit_price AS total FROM new_rows
                  UNION ALL
                  SELECT order_id, -1, -(qty * unit_price) FROM old_rows) c
              GROUP BY order_id) d
        WHERE o.id = d.order_id;
    END IF;
    RETURN NULL;
END $$;

-- Transition tables allow only one event per trigger, hence three triggers.
CREATE TRIGGER order_items_ins AFTER INSERT ON order_items
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION order_items_recalc();
CREATE TRIGGER order_items_del AFTER DELETE ON order_items
    REFERENCING OLD TABLE AS old_rows
    FOR EACH STATEMENT EXECUTE FUNCTION order_items_recalc();
CREATE TRIGGER order_items_upd AFTER UPDATE ON order_items
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION order_items_recalc();

VACUUM ANALYZE;
