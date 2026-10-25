# Materialized Views Simulation - Line-by-Line Documentation

This document explains the `simulate_materialized_views` project: **what** each piece
does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Schema (init/01_schema.sql)](#schema-init01_schemasql)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

Analytics queries ("revenue per region for the last 30 days") scan and aggregate a lot
of rows. If a dashboard runs them on every page load, the database does the same work
over and over. A materialized view does the work once and stores the answer. The only
question is **how and when to refresh it** — that is what this project explores.

---

## Schema (init/01_schema.sql)

### The fact table

```sql
CREATE TABLE sales (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    region TEXT, product TEXT, amount NUMERIC(10,2), sold_at TIMESTAMPTZ
);
INSERT INTO sales ... FROM generate_series(1, 1000000);
```

One million rows over the past year, generated in SQL so first boot is fast. Identity
columns are the modern replacement for `SERIAL`.

### The materialized view

```sql
CREATE MATERIALIZED VIEW daily_sales_mv AS
SELECT region, sold_at::date AS day, count(*) AS orders, sum(amount) AS revenue
FROM sales GROUP BY region, sold_at::date;

CREATE UNIQUE INDEX daily_sales_mv_key ON daily_sales_mv (region, day);
```

1M rows collapse into ~1.8k (5 regions × 366 days). The unique index has two jobs: it
makes lookups fast, and it is **mandatory** for `REFRESH … CONCURRENTLY`, which
matches old and new rows by it.

### The incremental summary

```sql
CREATE TABLE daily_sales_agg (region, day, orders, revenue, PRIMARY KEY (region, day));
CREATE TABLE agg_watermark (name TEXT PRIMARY KEY, last_id BIGINT);
```

A regular table with the same shape as the MV, plus a watermark recording the highest
`sales.id` already folded in.

### Health check

The compose health check uses `pg_isready -h 127.0.0.1`. During first-boot init the
entrypoint runs a temporary server that listens only on a Unix socket, so a socket
check would report "ready" while the 1M-row seed is still running.

---

## Application Code (main.go)

### `resetDemo`

Deletes rows from earlier runs (`product = 'demo'`), clears the summary table, resets
the watermark, and refreshes the MV. Every run starts from the same state.

### Read cost — `timeQuery`

Runs the raw aggregate and the MV query five times each and keeps the best time (warm
cache), so the comparison isn't skewed by the first cold read. Both return the same
answer; the MV reads ~150 rows instead of ~80k.

### Staleness — `insertSales` + `refresh`

1,000 rows dated today are inserted. `todayRevenue` reads the MV and sees the old total:
a materialized view is a snapshot. After `REFRESH MATERIALIZED VIEW` the total includes
the new rows.

### Locking — `readerDuringRefresh`

The refresh runs inside an open transaction so its lock is held while a second
connection tries to read with `SET LOCAL lock_timeout = '500ms'`.

| Command                | Lock taken          | Concurrent `SELECT` |
| ---------------------- | ------------------- | ------------------- |
| `REFRESH`              | `ACCESS EXCLUSIVE`  | waits (here: error `55P03` after 500 ms) |
| `REFRESH CONCURRENTLY` | `EXCLUSIVE`         | proceeds            |

`CONCURRENTLY` computes the new result into a temporary table, then applies only the
differences (`DELETE`/`INSERT`) using the unique index. It does more work overall but
never takes the view offline. `errors.AsType[*pgconn.PgError]` (Go 1.26) extracts the
SQLSTATE.

### Periodic refresh — `periodicRefresh`

Three goroutines run for 4 seconds: a `time.Ticker` refreshing concurrently every
second, a writer inserting sales, and a reader querying the MV. The counts show reads
are never blocked, and any write appears within about one interval. In production the
ticker would be cron, `pg_cron`, or a job queue.

### Incremental refresh — `incrementalRefresh`

In one transaction:

1. `SELECT last_id … FOR UPDATE` — read and lock the watermark (two workers can't run
   at once).
2. `SELECT max(id) FROM sales` — the new upper bound.
3. `MERGE` the aggregated new rows into `daily_sales_agg`:

```sql
MERGE INTO daily_sales_agg AS t
USING (SELECT region, sold_at::date AS day, count(*) AS orders, sum(amount) AS revenue
       FROM sales WHERE id > $1 AND id <= $2 GROUP BY 1, 2) AS s
ON t.region = s.region AND t.day = s.day
WHEN MATCHED THEN UPDATE SET orders = t.orders + s.orders, revenue = t.revenue + s.revenue
WHEN NOT MATCHED THEN INSERT VALUES (s.region, s.day, s.orders, s.revenue);
```

4. Move the watermark to the new upper bound.

Because the watermark moves in the same transaction as the merge, a crash can neither
double-count nor skip rows. Refreshing 1,000 new rows takes ~3 ms versus ~250 ms for a
full refresh — and the gap grows with table size.

**Limitations (be honest about them):** this handles inserts only. Updates and deletes
need triggers or CDC to emit deltas. And an id watermark can skip a row whose
transaction got its id early but committed after the refresh read `max(id)`; production
systems use a safety lag (process up to `max(id)` of transactions older than a few
seconds) or a commit-ordered change log.

### Verification — `verifyIncremental`

A symmetric `EXCEPT` between `daily_sales_agg` and `daily_sales_mv` returns zero rows:
the incremental table matches the full recompute exactly.

---

## Summary: Pros and Cons

**Pros**

- **Huge read speedups** for repeated aggregates — two orders of magnitude here.
- **Declarative** — one `CREATE MATERIALIZED VIEW`, indexable like a table.
- **Non-blocking refresh** with `CONCURRENTLY`.

**Cons**

- **Stale by design** — results lag the base tables until refreshed.
- **Full recompute** — PostgreSQL has no built-in incremental maintenance; refresh cost
  grows with the base table.
- **Storage and write amplification** — the result is stored and rewritten on refresh.
- **Incremental maintenance is DIY** — correct handling of updates, deletes and
  late-committing rows takes care.

**When to use:** read-heavy dashboards and reports that tolerate staleness. **When not
to:** data that must be up to the second, or base tables that change faster than you can
refresh.
