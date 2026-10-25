# Range-Based Sharding Simulation - Line-by-Line Documentation

This document explains the `simulate_range_based_sharding` project: **what** each piece
does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Schema](#infrastructure--schema)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

Sharding needs a rule from key to shard. Range sharding's rule is "find the range that
contains the key". It preserves key order across shards, which makes range scans cheap
and rebalancing granular — but it does nothing to spread load when keys arrive in order,
as timestamps do.

---

## Infrastructure & Schema

`docker-compose.yml` starts four identical PostgreSQL 18 shards (ports 5514–5517), using
YAML anchors (`&pg_env`, `&pg_health`) to avoid repeating config. `shard4` starts empty;
it receives half of a range when the demo splits one.

`init/01_schema.sql`:

```sql
CREATE TABLE orders (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    customer_id INT NOT NULL,
    amount NUMERIC(10,2) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX orders_created_at_idx ON orders (created_at);
```

- **`uuidv7()`** (new in PG18) produces ids that are unique across shards without
  coordination *and* roughly time-ordered, so B-tree inserts stay append-friendly
  (unlike random UUIDv4).
- **Index on `created_at`** — the shard key; range queries inside a shard become index
  range scans.

---

## Application Code (main.go)

### `Range`

```go
type Range struct {
    From, To time.Time // [From, To); To.IsZero() = unbounded
    Shards   []string
}
```

`contains` and `overlaps` implement half-open interval logic, so adjacent ranges never
both claim a boundary timestamp. `Shards` is a slice so the final step can spread one
range over several shards.

### `Cluster.route`

Finds the range containing `created_at`, then picks `Shards[customer % len(Shards)]`.
With one shard per range that's just the owner.

### `Cluster.shardsFor`

Returns every shard whose range overlaps the query interval — the **pruning** step. A
query for March of last year overlaps only the previous-year range, so only `shard2` is
visited. Hash sharding has no such shortcut: the query must fan out to all shards.

### `Cluster.insert`

Groups orders by target shard and loads each group with `CopyFrom`. The returned
per-shard counts drive the hotspot bar charts.

### History load

One order every 4 hours from January 1st two years ago until now (~6k rows). Year ranges
receive similar counts, except the current year, which is partial — ranges fill at
different speeds, the first hint that they'll need rebalancing.

### Write hotspot

`burst(now, 1500)` simulates 1,500 customers ordering right now. Every row's
`created_at` falls in the open-ended newest range, so **one shard takes 100% of writes**
while the others are idle. This is inherent to range sharding on a monotonic key.

### `splitRange`

1. Find the range containing the split point.
2. In a source transaction: `DELETE FROM orders WHERE created_at >= $1 RETURNING …` —
   removing and collecting the rows in one statement.
3. `CopyFrom` them into the new shard (committed independently).
4. Commit the source transaction. If the copy failed, the deferred `Rollback` restores
   the deleted rows, so data is never lost (a crash between 3 and 4 would leave
   duplicates on the target, which a real tool would detect and clean up).
5. Replace the range in the map with `[From, at)` on the old shard and `[at, To)` on the
   new one (`slices.Replace`).

Only rows in the upper half move; shards 1 and 2 are not touched. Compare with modulo
hashing, where adding a shard moves keys on *every* shard.

### Hotspot again

The next burst goes 100% to `shard4`: splitting moved the open-ended range, it didn't
divide the write load. Splitting fixes **size** imbalance, not **append** imbalance.

### Mitigation — hash-prefixed current range

The open range is closed at `cut`, and a new range `[cut, ∞)` is mapped to
`shard3 + shard4`, with rows placed by `customer_id % 2`. New writes now split roughly
50/50. Existing data needs no move because the change applies only from `cut` onwards.
The trade-off is printed: queries on recent data must now visit both shards.

---

## Summary: Pros and Cons

**Pros**

- **Efficient range queries** — visit only the shards whose ranges overlap.
- **Locality** — related keys (same period, same alphabetic prefix) stay together.
- **Granular rebalancing** — split one range, move one half.
- **Easy lifecycle management** — old ranges can be archived or dropped as a unit.

**Cons**

- **Hotspots** — sequential keys concentrate writes on one shard.
- **Uneven growth** — ranges must be monitored and split/merged.
- **Range map management** — the map must be stored, versioned and distributed to all
  routers (here it's in-memory; production systems keep it in a metadata store).

**When to use:** scan-heavy access by key order, time-based retention. **When not to:**
write-heavy append workloads on a monotonic key without bucketing — use hash sharding or
consistent hashing there.
