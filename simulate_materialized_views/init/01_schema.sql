-- MATERIALIZED VIEWS: store the RESULT of an expensive query as a table-like object
-- and serve reads from it. The price is staleness: the result only changes when the
-- view is refreshed.

-- Raw fact table: one row per sale.
CREATE TABLE sales (
    id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    region   TEXT NOT NULL,
    product  TEXT NOT NULL,
    amount   NUMERIC(10, 2) NOT NULL,
    sold_at  TIMESTAMPTZ NOT NULL
);

-- 1,000,000 sales spread over the last 365 days, 5 regions, 50 products.
INSERT INTO sales (region, product, amount, sold_at)
SELECT (ARRAY['north', 'south', 'east', 'west', 'central'])[1 + (g % 5)],
       'product-' || (g % 50),
       round((random() * 200 + 5)::numeric, 2),
       now() - (random() * interval '365 days')
FROM generate_series(1, 1000000) AS g;

ANALYZE sales;

-- The expensive query, precomputed: revenue per region per day.
CREATE MATERIALIZED VIEW daily_sales_mv AS
SELECT region,
       sold_at::date AS day,
       count(*)      AS orders,
       sum(amount)   AS revenue
FROM sales
GROUP BY region, sold_at::date;

-- A UNIQUE index is REQUIRED for REFRESH ... CONCURRENTLY: PostgreSQL diffs the old
-- and new results row-by-row using it. It also makes lookups by (region, day) fast.
CREATE UNIQUE INDEX daily_sales_mv_key ON daily_sales_mv (region, day);

-- INCREMENTAL alternative: a plain summary table that we update with only the NEW
-- rows since the last run (tracked by a watermark), instead of recomputing everything.
CREATE TABLE daily_sales_agg (
    region  TEXT NOT NULL,
    day     DATE NOT NULL,
    orders  BIGINT NOT NULL,
    revenue NUMERIC NOT NULL,
    PRIMARY KEY (region, day)
);

CREATE TABLE agg_watermark (
    name    TEXT PRIMARY KEY,
    last_id BIGINT NOT NULL
);
INSERT INTO agg_watermark VALUES ('daily_sales_agg', 0);
