# Query Caching Simulation - Line-by-Line Documentation

This document explains the `simulate_query_caching` project: **what** each piece
does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure (docker-compose.yml)](#infrastructure-docker-composeyml)
3. [Schema (init/01_create_schema.sql)](#schema-init01_create_schemasql)
4. [Application Code (main.go)](#application-code-maingo)
5. [Summary: Pros and Cons of Query Caching](#summary-pros-and-cons-of-query-caching)

---

## Project Overview

Databases are durable but comparatively slow; memory caches are fast but volatile.
**Query caching** keeps the hot, frequently-read results in a fast in-memory store
(Redis) so the database is consulted only on a **cache miss**. The hard part is
not the speed-up — it's keeping the cache from serving **stale** data after a
write. This project shows the standard answers: TTLs, write-through, and
invalidation.

---

## Infrastructure (docker-compose.yml)

### PostgreSQL

```yaml
postgres:
  image: postgres:18-bookworm
  ports:
    - "5441:5432"
```

Standard PostgreSQL. The only twist lives in the init script: a function that
sleeps 50 ms per call to imitate an expensive query, so the cache benefit is
measurable.

### Redis

```yaml
redis:
  image: redis:7-alpine
  ports:
    - "6379:6379"
  healthcheck:
    test: ["CMD-SHELL", "redis-cli ping | grep -q PONG"]
```

| Line             | Why We Do It                                            |
| ---------------- | ------------------------------------------------------- |
| `redis:7-alpine` | Tiny, fast, ubiquitous cache image                      |
| `redis-cli ping` | Healthcheck so the app waits until Redis answers `PONG` |

---

## Schema (init/01_create_schema.sql)

```sql
CREATE FUNCTION get_product_slow(p_id INTEGER) ... AS $$
BEGIN
    PERFORM pg_sleep(0.05);   -- 50ms of "expensive" work
    RETURN QUERY SELECT ... FROM products WHERE id = p_id;
END;
$$;
```

| Element          | Why We Do It                                                        |
| ---------------- | ------------------------------------------------------------------- |
| `pg_sleep(0.05)` | Simulates a heavy join/aggregation so cache hits are visibly faster |
| `RETURN QUERY`   | Returns the product row like a normal query                         |

In a real system you would cache a genuinely expensive query; the sleep is only a
teaching device.

---

## Application Code (main.go)

### The Cache type

```go
type Cache struct {
    db     *pgxpool.Pool
    rdb    *redis.Client
    ttl    time.Duration
    hits   int
    misses int
}
```

The `Cache` owns both backends and the caching policy. `hits`/`misses` track the
**hit ratio**, the single most important caching metric.

### GetProduct — cache-aside (lazy loading)

```go
if raw, err := c.rdb.Get(ctx, cacheKey(id)).Result(); err == nil {
    json.Unmarshal([]byte(raw), &p); c.hits++; return &p, ...
} else if !errors.Is(err, redis.Nil) {
    log.Printf("redis error (falling back to DB): %v", err)
}

// miss -> DB
c.misses++
c.db.QueryRow(ctx, "SELECT ... FROM get_product_slow($1)", id).Scan(...)
c.rdb.Set(ctx, cacheKey(id), data, c.ttl)   // populate
```

| Step                             | What It Does                            | Why We Do It                                                 |
| -------------------------------- | --------------------------------------- | ------------------------------------------------------------ |
| `rdb.Get`                        | Look in the cache first                 | The whole point — avoid the DB when possible                 |
| `errors.Is(err, redis.Nil)`      | Distinguish "missing" from "Redis down" | A true miss is normal; a Redis error means degrade to the DB |
| else-branch `log` + fall through | Graceful degradation                    | A cache outage must not take the app down                    |
| `json.Unmarshal` / `Marshal`     | Serialize the row                       | Redis stores bytes/strings, not Go structs                   |
| `rdb.Set(..., c.ttl)`            | Populate with a TTL                     | Bounds how stale a value can ever get                        |

**Why cache-aside?** It's simple, resilient (cache failures are survivable), and
only caches data that is actually requested.

### UpdatePriceWriteThrough — write-through

```go
c.db.QueryRow(ctx, "UPDATE products SET price=$1 WHERE id=$2 RETURNING ...", ...).Scan(&p...)
c.rdb.Set(ctx, cacheKey(id), data, c.ttl)
```

Write to the DB and immediately refresh the cache with the new row. The next read
is a **hit** _and_ already correct. Trade-off: you pay a cache write on every
update, even for keys nobody reads.

### UpdatePriceInvalidate — write + invalidate

```go
c.db.Exec(ctx, "UPDATE products SET price=$1 WHERE id=$2", ...)
c.rdb.Del(ctx, cacheKey(id))
```

Write to the DB and **delete** the cache entry. The next read misses and reloads
fresh data. This is often preferred over write-through because it's simpler and
avoids caching values that may never be read again.

| Approach         | Pro                              | Con                                   |
| ---------------- | -------------------------------- | ------------------------------------- |
| Write-through    | Next read is a fast, fresh hit   | Caches even unread keys; double write |
| Write+invalidate | Simple; only re-caches on demand | Next read pays one miss penalty       |

### demonstrateTTL — expiry

```go
c.GetProduct(ctx, 4)               // populate
c.rdb.Expire(ctx, key, 1*time.Second)
time.Sleep(1200 * time.Millisecond)
c.rdb.Exists(ctx, key)             // -> 0
```

Shows that TTL'd entries vanish on their own. TTL is the safety net: even if an
invalidation is missed (bug, race, crash), staleness is capped at the TTL.

### main — setup and flush

```go
rdb.FlushDB(ctx)   // start from an empty cache
cache := &Cache{db: db, rdb: rdb, ttl: 30 * time.Second}
```

`FlushDB` makes the demo reproducible; you would **never** do this in production.

---

## Summary: Pros and Cons of Query Caching

### Pros

| Benefit                | Explanation                                             |
| ---------------------- | ------------------------------------------------------- |
| **Latency**            | In-memory hits are orders of magnitude faster than disk |
| **Database offload**   | Fewer queries reach the DB, freeing capacity            |
| **Cheap read scaling** | A cache node is cheaper than another DB replica         |
| **Resilience**         | A read cache can absorb traffic spikes                  |

### Cons

| Drawback                    | Explanation                                                    |
| --------------------------- | -------------------------------------------------------------- |
| **Stale data**              | The central challenge; needs TTLs/invalidation                 |
| **Invalidation complexity** | "There are only two hard things..." — getting it right is hard |
| **Extra component**         | Another system to run, secure, and monitor                     |
| **Cold cache / stampede**   | After a flush, many misses can hammer the DB at once           |

### When to Use Query Caching

**Good fit:** read-heavy, repetitive queries that tolerate slight staleness
(catalogs, profiles, config, expensive aggregations). **Be careful with:** data
that must always be exact (balances, inventory at checkout) — there, cache with
very short TTLs or write-through, or don't cache at all.

## Key Takeaways

1. Cache-aside is the default: read cache, miss → DB, then populate.
2. Always set a TTL — it bounds staleness even when invalidation fails.
3. On writes, choose write-through (fresh hit) or invalidate (simple reload).
4. Treat the cache as optional: degrade to the DB if it's unavailable.
5. Watch the hit ratio — a low ratio means the cache isn't earning its keep.

## go run output

```shell
$ go run main.go
✅ Connected to PostgreSQL and Redis

============================================================
🗃️  QUERY CACHING SIMULATION (cache-aside)
============================================================

❄️  COLD READ (cache miss) vs 🔥 WARM READ (cache hit)
--------------------------------------------------
   Cold read product #1 (Mechanical Keyboard): 57.452ms  [MISS -> DB]
   Warm read product #1:          99µs  [HIT  -> Redis]
   ⚡ Cache hit was ~579x faster

✍️  WRITE-THROUGH: keep the cache fresh on update
--------------------------------------------------
   Updated product #2 price to $24.99 (DB + cache in one step)
   Next read: $24.99 in 90µs (served from cache, already fresh)

🗑️  WRITE + INVALIDATE: drop the entry on update
--------------------------------------------------
   Updated product #3 to $199.00 and deleted its cache entry
   Next read: $199.00 in 50.511ms (MISS -> DB, cache repopulated)

⏳ TTL EXPIRY
--------------------------------------------------
   product #4 cached with TTL set to 1s
   After expiry, key present in Redis: false (next read will MISS)

📊 CACHE STATISTICS
--------------------------------------------------
   Hits: 2   Misses: 5   Hit ratio: 29%
```
