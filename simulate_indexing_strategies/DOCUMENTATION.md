# Indexing Strategies Simulation - Line-by-Line Documentation

This document explains the `simulate_indexing_strategies` project: **what** each piece
does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Schema (init/01_schema.sql)](#schema-init01_schemasql)
3. [Application Code (main.go)](#application-code-maingo)
4. [Index Types in Depth](#index-types-in-depth)
5. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

A query without a usable index reads the whole table. On 1M rows that's ~23,600 pages
(185 MB) per execution. An index turns that into a handful of pages. But an index only
helps if its **type supports the query's operator** and the planner judges it cheaper
— so the demo always shows the actual plan, not assumptions.

---

## Schema (init/01_schema.sql)

- **Extensions:** `pgstattuple` (for `pgstatindex()` — index density/bloat) and
  `btree_gist` (lets a GiST index include the scalar `room` column in the exclusion
  constraint).
- **`orders`** (1M rows) — inserted in `created_at` order, so the physical layout is
  correlated with time (what BRIN needs). Columns are shaped for specific index types:
  `customer_id` (B-tree), mixed-case `email` (hash/expression), skewed `status` with 1%
  `pending` (partial), `amount` (covering), `attrs` JSONB and `tags` array (GIN).
- **`reservations`** (100k rows) — room + `tstzrange` booking periods (GiST).
- No secondary indexes: the Go program creates each one.

---

## Application Code (main.go)

### `explain`

Runs `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` and walks the plan tree to the first
**scan** node, reporting its type, index name (looking one level down for a Bitmap
Heap Scan's Bitmap Index Scan), execution time, buffers touched, and heap fetches for
index-only scans. `before` and `after` run the query once to warm the cache, then
measure.

### `create`

Executes the `CREATE INDEX`, times it and prints `pg_relation_size` — so build cost and
size are visible next to the speedup.

### `reset`

Drops every demo index and the exclusion constraint, then `CLUSTER orders USING
orders_pkey` — rewriting the table in id (= time) order. That undoes bloat from the
previous run's bloat demo and restores the physical ordering BRIN depends on.
`pg_stat_reset()` zeroes usage counters for the unused-index check.

### `timeInserts`

Inserts 20,000 rows in one statement (then deletes them). Run first with only the
primary key and again with ten indexes, it shows the write cost of indexing.

### Bloat demo

Updates `customer_id` on a third of the rows twice. Each update inserts a new index
entry (the old row version's entry stays until VACUUM). After VACUUM the dead entries
are removed but the index doesn't shrink — `avg_leaf_density` drops and size doubles.
`REINDEX INDEX CONCURRENTLY` rebuilds a compact copy without blocking reads or writes,
then swaps it in.

### `printUsage`

Lists `pg_stat_user_indexes.idx_scan` per index, excluding unique indexes (they enforce
constraints even when never scanned). `idx_orders_amount`, added "just in case", has
zero scans — pure write and storage overhead.

---

## Index Types in Depth

### B-tree

Sorted tree; supports `= < <= > >= BETWEEN IN`, `IS NULL`, prefix `LIKE 'abc%'` (with
C collation or `text_pattern_ops`) and returns rows in order (no sort for `ORDER BY`).
The default for good reason.

### Hash

Stores a 32-bit hash per entry; supports only `=`. Crash-safe and replicated since
PG10. In this demo it's *larger* than the B-tree (short, repetitive keys) and can't
serve `ORDER BY email`. It can win for long keys (URLs, tokens) where B-tree entries are
big.

### Expression

`CREATE INDEX … ON orders (lower(email))` indexes the computed value. The query must use
the **same expression** — the plain `email` index is useless for `lower(email) = …`.

### Partial

`CREATE INDEX … ON orders (created_at) WHERE status = 'pending'` contains only 1% of rows:
240 kB versus 30 MB for `(status, created_at)`, with the same latency. Rows outside the
predicate are never added — cheaper writes too. The query's `WHERE` must imply the
index predicate.

### Covering (`INCLUDE`)

`(customer_id) INCLUDE (amount)` stores `amount` in leaf pages without making it part of
the key. The aggregate reads only the index (**Index Only Scan**), provided the
visibility map says pages are all-visible — hence the `VACUUM` before measuring
(`Heap Fetches: 0`).

### GIN

An inverted index: one entry per element/key → list of rows. Ideal for "contains"
operators: `jsonb @>`, `?`, arrays `@> && `, full-text `@@`. `jsonb_path_ops` is smaller
and faster for `@>` than the default `jsonb_ops` (which also supports key-existence `?`).
Results come as a Bitmap Heap Scan. GIN writes are relatively expensive (mitigated by
the pending list / `fastupdate`).

### GiST

A balanced tree of bounding "predicates" — supports overlap (`&&`), containment,
nearest-neighbour (`<->`) for ranges, geometry, and (with `btree_gist`) scalars. The
exclusion constraint `EXCLUDE USING gist (room WITH =, during WITH &&)` guarantees no
two bookings of the same room overlap, which a unique index cannot express. Inserting a
conflicting booking fails with SQLSTATE `23P01`.

### BRIN

Block Range INdex: stores min/max of `created_at` for each range of 128 pages. 24 kB for
1M rows. A range query skips block ranges whose min/max can't match — great when values
correlate with physical position (append-only time series), useless otherwise. Slower
than a B-tree because it reads whole block ranges, but at ~1/1000 the size.

---

## Summary: Pros and Cons

**Pros**

- Huge read reductions — 10–1000x fewer pages in this demo.
- Specialized types make otherwise-impossible queries fast (containment, overlap).
- Constraints: uniqueness, exclusion.

**Cons**

- Every index slows writes (7.5x for 10 indexes here) and uses disk + cache.
- Bloat requires maintenance (`REINDEX CONCURRENTLY`, autovacuum tuning).
- Wrong type or expression → silently unused. Always verify with `EXPLAIN`.

**When to use:** index for your actual query patterns, measured. **When not to:** columns
never filtered/sorted on, tiny tables, write-heavy tables where a rarely-run query
doesn't justify the overhead.
