-- INDEXING STRATEGIES: tables are created WITHOUT secondary indexes. The Go program
-- adds each index type in turn and compares query plans before and after.

-- pgstattuple: inspect index density/bloat. btree_gist: lets GiST index plain
-- scalar columns (needed for the exclusion constraint on room + time range).
CREATE EXTENSION pgstattuple;
CREATE EXTENSION btree_gist;

CREATE TABLE orders (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id INT         NOT NULL,
    email       TEXT        NOT NULL,
    status      TEXT        NOT NULL,
    amount      NUMERIC(10, 2) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    attrs       JSONB       NOT NULL,
    tags        TEXT[]      NOT NULL
);

-- 1,000,000 rows inserted in created_at order (one every 30 s) — physical order
-- correlates with time, which is what makes BRIN effective.
INSERT INTO orders (customer_id, email, status, amount, created_at, attrs, tags)
SELECT g % 100000,
       'User' || (g % 200000) || '@Example.com',            -- mixed case on purpose
       CASE WHEN g % 100 = 0 THEN 'pending'                 -- 1%  pending
            WHEN g % 50  = 0 THEN 'cancelled'               -- 1%  cancelled
            ELSE 'done' END,                                -- 98% done
       round((random() * 500)::numeric, 2),
       timestamptz '2025-01-01' + g * interval '30 seconds',
       jsonb_build_object(
           'channel',  (ARRAY['web', 'ios', 'android', 'pos'])[1 + g % 4],
           'priority', g % 97 = 0,
           'country',  (ARRAY['ID', 'SG', 'MY', 'TH', 'VN', 'PH'])[1 + g % 6]),
       ARRAY['tag' || (g % 50), 'tag' || (50 + g % 7)]
FROM generate_series(1, 1000000) g;

-- Room bookings with time ranges: the classic GiST use case.
CREATE TABLE reservations (
    id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room   INT       NOT NULL,
    during TSTZRANGE NOT NULL
);

-- 1,000 rooms × 100 non-overlapping 2-hour bookings each.
INSERT INTO reservations (room, during)
SELECT r, tstzrange(timestamptz '2025-06-01' + (b * interval '3 hours'),
                    timestamptz '2025-06-01' + (b * interval '3 hours') + interval '2 hours')
FROM generate_series(1, 1000) r, generate_series(0, 99) b;

VACUUM ANALYZE;
