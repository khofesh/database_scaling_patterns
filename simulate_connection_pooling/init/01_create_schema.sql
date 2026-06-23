-- A tiny accounts table used by the concurrency demo. The workload is
-- deliberately simple (point reads/updates) so the bottleneck we observe is
-- connection management, not query complexity.

CREATE TABLE accounts (
    id       SERIAL PRIMARY KEY,
    owner    VARCHAR(100) NOT NULL,
    balance  DECIMAL(12, 2) NOT NULL DEFAULT 0
);

INSERT INTO accounts (owner, balance)
SELECT 'user_' || g, (g * 10)::numeric
FROM generate_series(1, 50) AS g;
