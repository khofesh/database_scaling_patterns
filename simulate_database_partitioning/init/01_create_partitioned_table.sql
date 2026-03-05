-- Create partitioned table for orders (partitioned by date range)
CREATE TABLE orders (
    id SERIAL,
    order_date DATE NOT NULL,
    customer_id INTEGER NOT NULL,
    amount DECIMAL(10, 2) NOT NULL,
    status VARCHAR(50) DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id, order_date)
) PARTITION BY RANGE (order_date);

-- Create partitions for different date ranges
CREATE TABLE orders_2024_q1 PARTITION OF orders
    FOR VALUES FROM ('2024-01-01') TO ('2024-04-01');

CREATE TABLE orders_2024_q2 PARTITION OF orders
    FOR VALUES FROM ('2024-04-01') TO ('2024-07-01');

CREATE TABLE orders_2024_q3 PARTITION OF orders
    FOR VALUES FROM ('2024-07-01') TO ('2024-10-01');

CREATE TABLE orders_2024_q4 PARTITION OF orders
    FOR VALUES FROM ('2024-10-01') TO ('2025-01-01');

CREATE TABLE orders_2025_q1 PARTITION OF orders
    FOR VALUES FROM ('2025-01-01') TO ('2025-04-01');

CREATE TABLE orders_2025_q2 PARTITION OF orders
    FOR VALUES FROM ('2025-04-01') TO ('2025-07-01');

CREATE TABLE orders_2025_q3 PARTITION OF orders
    FOR VALUES FROM ('2025-07-01') TO ('2025-10-01');

CREATE TABLE orders_2025_q4 PARTITION OF orders
    FOR VALUES FROM ('2025-10-01') TO ('2026-01-01');

CREATE TABLE orders_2026_q1 PARTITION OF orders
    FOR VALUES FROM ('2026-01-01') TO ('2026-04-01');

CREATE TABLE orders_2026_q2 PARTITION OF orders
    FOR VALUES FROM ('2026-04-01') TO ('2026-07-01');

-- Create indexes on partitions
CREATE INDEX idx_orders_customer_id ON orders (customer_id);
CREATE INDEX idx_orders_status ON orders (status);

-- Create a hash-partitioned table for users (partitioned by user_id)
CREATE TABLE users (
    id INTEGER NOT NULL,
    username VARCHAR(100) NOT NULL,
    email VARCHAR(255) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
) PARTITION BY HASH (id);

-- Create hash partitions
CREATE TABLE users_p0 PARTITION OF users FOR VALUES WITH (MODULUS 4, REMAINDER 0);
CREATE TABLE users_p1 PARTITION OF users FOR VALUES WITH (MODULUS 4, REMAINDER 1);
CREATE TABLE users_p2 PARTITION OF users FOR VALUES WITH (MODULUS 4, REMAINDER 2);
CREATE TABLE users_p3 PARTITION OF users FOR VALUES WITH (MODULUS 4, REMAINDER 3);

-- Create a list-partitioned table for products (partitioned by category)
CREATE TABLE products (
    id SERIAL,
    name VARCHAR(255) NOT NULL,
    category VARCHAR(50) NOT NULL,
    price DECIMAL(10, 2) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id, category)
) PARTITION BY LIST (category);

-- Create list partitions
CREATE TABLE products_electronics PARTITION OF products FOR VALUES IN ('electronics', 'computers', 'phones');
CREATE TABLE products_clothing PARTITION OF products FOR VALUES IN ('clothing', 'shoes', 'accessories');
CREATE TABLE products_home PARTITION OF products FOR VALUES IN ('furniture', 'kitchen', 'garden');
CREATE TABLE products_other PARTITION OF products DEFAULT;
