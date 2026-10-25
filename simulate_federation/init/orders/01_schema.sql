-- ORDERS database: owned by the "commerce" domain/team. It references users only by
-- id — there is NO foreign key, because the users table lives in another database.
CREATE TABLE orders (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL,           -- logical reference to users_db.users.id
    amount     NUMERIC(10, 2) NOT NULL,
    status     TEXT NOT NULL DEFAULT 'paid',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON orders (user_id);
CREATE INDEX ON orders (created_at);

INSERT INTO orders (user_id, amount, created_at)
SELECT 1 + (random() * 4999)::int, round((random() * 200 + 5)::numeric, 2),
       now() - random() * interval '90 days'
FROM generate_series(1, 50000);
