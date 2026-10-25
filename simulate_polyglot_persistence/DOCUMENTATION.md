# Polyglot Persistence Simulation - Line-by-Line Documentation

This document explains the `simulate_polyglot_persistence` project: **what** each piece
does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Schema](#infrastructure--schema)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

Relational databases are excellent general-purpose stores, but some workloads have
better-suited engines: inverted indexes with fuzzy matching for search, in-memory data
structures for counters and caches. Polyglot persistence adds those engines **next to**
the relational database — keeping it as the system of record — and introduces the core
problem of the pattern: keeping copies of the same data consistent across systems that
share no transactions.

---

## Infrastructure & Schema

### docker-compose.yml

- **`postgres`** (5535) — system of record.
- **`redis`** (6380) — Redis 8; derived data only, so no persistence is configured.
- **`elasticsearch`** (9200) — single node, `xpack.security.enabled=false` (demo only),
  512 MB heap; health check waits for cluster status yellow.

### init/01_schema.sql

- **`products.version`** — incremented on every update. Downstream stores use it to
  reject stale or replayed updates.
- **`orders`** — feeds the leaderboard.
- **`outbox (id, event, payload JSONB, created_at, processed_at)`** plus a partial index
  `WHERE processed_at IS NULL`: the relay only scans pending events, so the index stays
  tiny as processed history grows.

---

## Application Code (main.go)

### Writes — PostgreSQL + outbox only

`seedProducts`, `UpdateProduct` and `placeRandomOrders` run inside `pgx.BeginFunc`. Each
business write is followed by `emit(…)`, an `INSERT INTO outbox` in the **same
transaction**. Either both commit or neither does, so an event exists exactly when the
change happened. The application never calls Redis or Elasticsearch on the write path —
eliminating the dual-write failure mode ("PostgreSQL committed, then the Elasticsearch
call failed").

`UpdateProduct` uses `UPDATE … SET version = version + 1 … RETURNING *` so the event
carries a full snapshot with the new version.

### The relay — `Relay.processBatch`

In one PostgreSQL transaction:

1. `SELECT … FROM outbox WHERE processed_at IS NULL ORDER BY id LIMIT 500 FOR UPDATE
   SKIP LOCKED` — claim a batch. `SKIP LOCKED` lets several relay instances run without
   blocking each other.
2. For `product_upserted`: collect documents for one Elasticsearch `_bulk` request and
   queue `DEL product:<id>` in a Redis pipeline (cache invalidation).
3. For `order_placed`: queue the `incrOnce` Lua script.
4. Send the bulk request and execute the Redis pipeline.
5. `UPDATE outbox SET processed_at = now() WHERE id = ANY($1)` and commit.

If step 4 fails, the transaction rolls back and the same events are retried later:
**at-least-once** delivery. That's safe only because every applier is **idempotent**:

- **Elasticsearch external versioning** — each bulk action carries `"version":
  product.version, "version_type": "external"`. Elasticsearch stores the document only if
  the version is higher than the stored one; a replay or an out-of-order older event gets
  `409 Conflict`, which `BulkIndex` treats as success.
- **Redis dedupe** — the Lua script `SET outbox:done:<id> 1 NX EX 86400` and, only if that
  succeeded, `ZINCRBY bestsellers qty product`. Scripts run atomically, so a retried event
  never double-counts.
- **Cache `DEL`** is naturally idempotent.

`Start`/`Stop` run the loop in a goroutine (`sync.WaitGroup.Go`) with a cancellable
context; when idle, it polls every 200 ms. (Production relays often use `LISTEN/NOTIFY`
to wake up immediately, or CDC instead of polling.)

### Search — `ES.Search`

```json
{"multi_match": {"query": "wireles headphnes", "fields": ["name^3", "description"],
                 "fuzziness": "AUTO", "operator": "and"},
 "aggs": {"by_category": {"terms": {"field": "category"}}}}
```

`fuzziness: AUTO` allows 1–2 character edits depending on term length; `name^3` weights
name matches higher for relevance; the `terms` aggregation returns facet counts. The
mapping uses the **standard** analyzer: fuzzy matching runs against indexed terms, and a
stemmer would turn `wireles` into `wirel` — too far from `wireless` to match.

For comparison, PostgreSQL `ILIKE '%wireles headphnes%'` and
`to_tsvector(...) @@ plainto_tsquery(...)` both return 0 rows: neither tolerates typos.
(`pg_trgm` similarity search can help in PostgreSQL, but without BM25 relevance and
facets.)

### Cache-aside — `GetProduct`

`GET product:<id>` from Redis; on a miss, load from PostgreSQL and `SET` with a 10-minute
TTL. The relay deletes the key whenever the product changes, so the next read refills it
with fresh data. The TTL is a safety net in case an invalidation is ever missed.

### Leaderboard

`ZREVRANGE bestsellers 0 4 WITHSCORES` reads the top five directly from a sorted set
maintained incrementally by the relay — O(log n) per order. The equivalent PostgreSQL
`GROUP BY` re-aggregates every order on each read; both are fast at 2,000 orders, but only
one stays fast with millions.

### Eventual consistency

Immediately after `UpdateProduct` commits, PostgreSQL shows the new name while
Elasticsearch (`GET /products/_doc/42`, a real-time get) still has the old one.
`waitDrained` polls until the outbox is empty, then calls `_refresh` so searches see the
change too. The lag is the relay's poll interval plus processing time — bounded and
observable (count of pending outbox rows).

### Relay outage

`relay.Stop()`, then 50 updates: all commit to PostgreSQL, and 50 events wait in the
outbox. The application is unaffected. `relay.Start()` drains the backlog in one batch.
Compare with dual writes, where an Elasticsearch outage would fail or silently drop
those updates.

### Rebuild — `reindex`

`ES.Reset()` drops the index; `reindex` reads all products from PostgreSQL
(`pgx.RowToStructByPos[Product]`) and bulk-loads them in chunks of 1,000
(`slices.Chunk`). Because PostgreSQL is the source of truth, any derived store can be
rebuilt — after a mapping change, corruption, or a new store being added.

### Minimal Elasticsearch client

`ES.do` wraps `net/http` and `encoding/json`, sending NDJSON for `_bulk`. A full client
library isn't needed for a handful of REST calls, and the raw requests make the
Elasticsearch API visible.

---

## Summary: Pros and Cons

**Pros**

- **Best tool per workload** — real search, sub-millisecond cache, live counters.
- **Offloads the primary database** — searches and hot reads no longer hit PostgreSQL.
- **Resilient sync** — the outbox decouples write availability from derived stores.
- **Rebuildable** — derived stores can be dropped and repopulated.

**Cons**

- **Operational load** — three systems to deploy, secure, upgrade, monitor, back up.
- **Eventual consistency** — reads from derived stores can be briefly stale.
- **Sync pipeline** — relay or CDC must be reliable, idempotent and monitored (backlog).
- **Broader expertise** required across the team.

**When to use:** when a specific access pattern clearly outgrows PostgreSQL. **When not
to:** when PostgreSQL features (full-text search, `pg_trgm`, JSONB, materialized views,
partitioning) still meet the requirement — one system is simpler.
