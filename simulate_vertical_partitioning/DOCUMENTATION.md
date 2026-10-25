# Vertical Partitioning Simulation - Line-by-Line Documentation

This document explains the `simulate_vertical_partitioning` project: **what** each
piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Schema (init/01_schema.sql)](#schema-init01_schemasql)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

PostgreSQL stores rows in 8 KB pages. Everything that reads or writes data — sequential
scans, the buffer cache, WAL, replication — works in rows and pages. So the **width of a
row** directly sets how many rows fit in a page, how much memory the working set needs,
and how many bytes each update writes. Vertical partitioning narrows the hot rows by
moving columns that most queries don't need into separate tables.

### How TOAST fits in

PostgreSQL already moves large values out of line via **TOAST** when a row exceeds
~2 KB (`TOAST_TUPLE_THRESHOLD`): it compresses and/or stores them in a side table,
leaving an 18-byte pointer. That's automatic vertical partitioning for *big* values — but:

- Mid-size values (a 1 KB bio, a 300 B JSON blob) stay **inline**, widening every row.
- `SELECT *` still de-TOASTs and transfers big values.
- TOAST is per value and invisible to you; explicit splitting is a design decision you
  can align with access patterns (and later move to another store).

---

## Schema (init/01_schema.sql)

### `users_wide` — before

Eight columns: hot (`email`, `name`, `status`, `last_login`), cold (`bio` ≈ 1 KB,
`preferences` ≈ 300 B JSONB) and BLOB (`avatar` = 4 KB random bytes). The row is ~1.4 KB
inline (bio and preferences below the TOAST threshold) plus a TOAST pointer for the
avatar, so only ~5 rows fit per page.

The avatar is built from 256 random UUIDs (`uuid_send(gen_random_uuid())`), giving
incompressible bytes like a real JPEG. The `g * 0` term makes the subquery reference the
outer row so it is evaluated per user.

### The split — after

```sql
CREATE TABLE users_core    (id BIGINT PRIMARY KEY, email, name, status, last_login);
CREATE TABLE users_profile (user_id BIGINT PRIMARY KEY REFERENCES users_core ON DELETE CASCADE, bio, preferences);
CREATE TABLE users_avatar  (user_id BIGINT PRIMARY KEY REFERENCES users_core ON DELETE CASCADE, avatar BYTEA);
ALTER TABLE users_avatar ALTER COLUMN avatar SET STORAGE EXTERNAL;
```

- **Same primary key** in all three tables → reassembly is a PK-to-PK join.
- **`ON DELETE CASCADE`** keeps the parts consistent when a user is deleted.
- **`STORAGE EXTERNAL`** — store out of line *without* trying to compress. Compression
  attempts on already-compressed images waste CPU.

### `users_full` view

`LEFT JOIN`s the three tables so code that needs a whole user can still query one
relation. `VACUUM ANALYZE` at the end gives the planner accurate statistics and sets
the visibility map.

---

## Application Code (main.go)

### `printSizes`

Uses `pg_relation_size` (heap only) and `pg_table_size − pg_relation_size` (TOAST + free
space map + visibility map) plus `reltuples / relpages` for rows per page. Note that
`users_wide`'s 47 MB heap is almost entirely bio/preferences — the avatars are in its
158 MB TOAST table.

### `explain` and `compare`

`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` returns the number of shared buffers hit or
read — i.e. pages touched — plus execution time. The JSON is decoded into a small struct.
Each query runs once to warm the cache, then again for measurement.

- **Hot query** (`count(*)` of recently active users) and **listing** (top 50 by
  `last_login`) both scan the table. The wide table touches ~6,000 pages, the core table
  ~300. On a warm cache that's ~2–3x faster; on a cold cache or under memory pressure it
  is the difference between reading 49 MB and 2.6 MB from disk.

### `selectStar`

Runs `SELECT * … LIMIT 2000` and totals `rows.RawValues()` lengths — the exact bytes
received, without decoding. The wide table ships every avatar: ~10.8 MB versus 0.1 MB.
This is the cost ORMs silently impose with default `SELECT *` mappings.

### `timeOne` — profile page

Fetching one full user from `users_wide` touches 3 pages; via `users_full` it touches 9
(three PK index lookups). Still a few microseconds. The join cost is paid only on the
rare path that needs everything.

### `walFor` — write amplification

Records `pg_current_wal_lsn()`, updates `last_login` for 10,000 users, and computes
`pg_wal_lsn_diff`. PostgreSQL's MVCC writes a **new version of the whole row** on update,
so the wide table generates several times more WAL — which also means more replication
traffic and more bloat for VACUUM to clean. The absolute numbers vary between runs
because the first change to each page after a checkpoint writes a full-page image. A
`VACUUM` afterwards reclaims dead row versions so re-runs start from a similar state.

---

## Summary: Pros and Cons

**Pros**

- **Smaller hot working set** — more rows per page and in cache.
- **Cheaper scans and `SELECT *`** on the hot table.
- **Less write amplification** — updates rewrite narrow rows.
- **Independent evolution** — BLOBs can be moved to object storage later.

**Cons**

- **Joins to reassemble** the full entity.
- **Multi-table writes** — inserts must populate several tables in one transaction.
- **More schema to maintain** and map in the ORM.
- **Little gain** when rows are already narrow or every query needs all columns.

**When to use:** wide tables with a clear split between frequently and rarely accessed
columns, especially with large text/binary payloads. **When not to:** narrow tables or
uniform access to all columns.
