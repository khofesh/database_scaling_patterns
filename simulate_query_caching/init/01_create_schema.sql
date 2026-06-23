-- Products catalog used by the caching demo.

CREATE TABLE products (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(200) NOT NULL,
    category    VARCHAR(100) NOT NULL,
    price       DECIMAL(10, 2) NOT NULL,
    description TEXT
);

INSERT INTO products (name, category, price, description) VALUES
    ('Mechanical Keyboard', 'peripherals', 89.99,  'Tactile switches, hot-swappable'),
    ('Wireless Mouse',      'peripherals', 29.50,  'Ergonomic, 2.4GHz + Bluetooth'),
    ('27-inch Monitor',     'displays',    219.00, '1440p, 165Hz IPS'),
    ('USB-C Hub',           'accessories', 39.99,  '7-in-1 with HDMI and PD'),
    ('Laptop Stand',        'accessories', 49.99,  'Aluminium, adjustable height');

-- Simulate an expensive read (a heavy join/aggregation in real life) by sleeping
-- briefly before returning a product. This makes the cache speed-up visible.
CREATE FUNCTION get_product_slow(p_id INTEGER)
RETURNS TABLE (id INTEGER, name VARCHAR, category VARCHAR, price DECIMAL, description TEXT)
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_sleep(0.05); -- 50ms of "expensive" work
    RETURN QUERY
        SELECT p.id, p.name, p.category, p.price, p.description
        FROM products p WHERE p.id = p_id;
END;
$$;
